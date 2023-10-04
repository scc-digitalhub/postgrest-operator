/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	postgrestv1 "github.com/scc-digitalhub/postgrest-operator/api/v1"
)

const postgrestImage = "POSTGREST_IMAGE"
const postgrestImageTag = "POSTGREST_IMAGE_TAG"
const postgrestServiceType = "POSTGREST_SERVICE_TYPE"
const postgrestDatabaseUri = "POSTGREST_DATABASE_URI"

const postgrestFinalizer = "postgrest.postgrest.digitalhub/finalizer" //TODO valutare nome

// Definitions to manage status conditions
const (
	// When anon role is being created, before deployment
	typeInitializing = "Initializing"

	// Launch deployment and service
	typeDeploying = "Deploying"

	typeRunning = "Running"

	typeError = "Error"

	// Custom resource is deleted, perform finalizer operations
	typeDegraded = "Degraded"
)

// PostgrestReconciler reconciles a Postgrest object
type PostgrestReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func formatResourceName(resourceName string) string {
	return strings.Join([]string{"postgrest", resourceName}, "-")
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,namespace=mynamespace,resources=postgrests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,namespace=mynamespace,resources=postgrests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,namespace=mynamespace,resources=postgrests/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,namespace=mynamespace,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *PostgrestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Postgrest instance
	// The purpose is check if the Custom Resource for the Kind Postgrest
	// is applied on the cluster if not we return nil to stop the reconciliation
	postgrest := &postgrestv1.Postgrest{}
	err := r.Get(ctx, req.NamespacedName, postgrest)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("Postgrest resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Postgrest")
		return ctrl.Result{}, err
	}

	// If status is unknown, set Initializing
	if postgrest.Status.State == "" {
		log.Info("State unspecified, updating to initializing")
		postgrest.Status.State = typeInitializing
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, "Failed to update Postgrest status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	if postgrest.Status.State == typeInitializing {
		log.Info("Creating anonymous role")
		err = createAnonRole(postgrest, ctx)
		if err != nil {
			log.Error(err, "Error while handling anonymous role")
			return ctrl.Result{}, err
		}

		postgrest.Status.State = typeDeploying
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, "Failed to update Postgrest status")
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	if postgrest.Status.State == typeDeploying {
		log.Info("Deploying and creating service")

		// Add finalizer
		if !controllerutil.ContainsFinalizer(postgrest, postgrestFinalizer) {
			log.Info("Adding finalizer for Postgrest")
			if ok := controllerutil.AddFinalizer(postgrest, postgrestFinalizer); !ok {
				log.Error(err, "Failed to add finalizer into the custom resource")
				return ctrl.Result{Requeue: true}, nil
			}

			if err = r.Update(ctx, postgrest); err != nil {
				log.Error(err, "Failed to update custom resource to add finalizer")
				return ctrl.Result{}, err
			}

			if err := r.Get(ctx, req.NamespacedName, postgrest); err != nil {
				log.Error(err, "Failed to re-fetch Postgrest")
				return ctrl.Result{}, err
			}
		}

		existingSecret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, existingSecret)
		if err != nil && apierrors.IsNotFound(err) {
			// Create secret
			secret, err := r.secretForPostgrest(postgrest)
			if err != nil {
				log.Error(err, "Failed to define new Secret resource for Postgrest")

				postgrest.Status.State = typeError

				if err := r.Status().Update(ctx, postgrest); err != nil {
					log.Error(err, "Failed to update Postgrest status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
			log.Info("Creating a new Secret", "Secret.Namespace", secret.Namespace, "Secret.Name", secret.Name)
			if err = r.Create(ctx, secret); err != nil {
				log.Error(err, "Failed to create new Secret", "Secret.Namespace", secret.Namespace, "Secret.Name", secret.Name)
				return ctrl.Result{}, err
			}
		} else if err != nil {
			log.Error(err, "Failed to get secret")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		// Check if the deployment already exists, if not create a new one
		found := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, found)
		if err != nil && apierrors.IsNotFound(err) {

			// Define a new deployment
			dep, err := r.deploymentForPostgrest(postgrest)
			if err != nil {
				log.Error(err, "Failed to define new Deployment resource for Postgrest")

				postgrest.Status.State = typeError

				if err := r.Status().Update(ctx, postgrest); err != nil {
					log.Error(err, "Failed to update Postgrest status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}

			log.Info("Creating a new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			if err = r.Create(ctx, dep); err != nil {
				log.Error(err, "Failed to create new Deployment",
					"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
				return ctrl.Result{}, err
			}
		} else if err != nil {
			log.Error(err, "Failed to get deployment")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		// Create service
		existingService := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, existingService)
		if err != nil && apierrors.IsNotFound(err) {
			service, err := r.serviceForPostgrest(postgrest)
			if err != nil {
				log.Error(err, "Service inizialition failed")

				postgrest.Status.State = typeError

				if err := r.Status().Update(ctx, postgrest); err != nil {
					log.Error(err, "Failed to update Postgrest status")
					return ctrl.Result{}, err
				}

				return ctrl.Result{}, err
			}
			log.Info("Creating a new Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
			if err = r.Create(ctx, service); err != nil {
				log.Error(err, "Service creation failed")
				return ctrl.Result{}, err
			}
		} else if err != nil {
			log.Error(err, "Failed to get service")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		postgrest.Status.State = typeRunning
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, "Failed to update Postgrest status")
			return ctrl.Result{}, err
		}

		log.Info("Deployment and service created successfully")
		// Deployment created successfully
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Check if the Postgrest instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isPostgrestMarkedToBeDeleted := postgrest.GetDeletionTimestamp() != nil
	if isPostgrestMarkedToBeDeleted {
		log.Info("Resource marked to be deleted")
		if controllerutil.ContainsFinalizer(postgrest, postgrestFinalizer) {
			log.Info("Performing Finalizer Operations for Postgrest before delete CR")

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			if err := r.doFinalizerOperationsForPostgrest(postgrest); err != nil {
				log.Error(err, "Finalizer operations failed")
				return ctrl.Result{Requeue: true}, nil
			}

			// Re-fetch the Postgrest Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, postgrest); err != nil {
				log.Error(err, "Failed to re-fetch Postgrest")
				return ctrl.Result{}, err
			}

			postgrest.Status.State = typeDegraded

			if err := r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, "Failed to update Postgrest status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Postgrest after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(postgrest, postgrestFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Postgrest")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, postgrest); err != nil {
				log.Error(err, "Failed to remove finalizer for Postgrest")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if postgrest.Status.State == typeRunning {
		dep := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, dep)
		if err != nil {
			log.Error(err, "Error while retrieving deployment")
			return ctrl.Result{}, err
		}

		// Deployment ready
		if dep.Status.ReadyReplicas > 0 {
			log.Info("Deployment is ready")
			if err = r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, "Failed to update Postgrest status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		// Wait up to 15 minutes for deployment to become ready
		if dep.CreationTimestamp.Add(15 * time.Minute).Before(time.Now()) {
			log.Info("Deployment still not ready, setting state to Error")
			postgrest.Status.State = typeError

			if err = r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, "Failed to update Postgrest status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if postgrest.Status.State == typeError {
		log.Info("Cleaning up secret, deployment and service")

		// Delete service
		service := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, service)
		if err == nil {
			if err := r.Delete(ctx, service); err != nil {
				log.Error(err, "Failed to clean up service")
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get service")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		// Delete deployment
		deployment := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, deployment)
		if err == nil {
			if err := r.Delete(ctx, deployment); err != nil {
				log.Error(err, "Failed to clean up deployment")
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get deployment")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		// Delete secret
		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, secret)
		if err == nil {
			if err := r.Delete(ctx, secret); err != nil {
				log.Error(err, "Failed to clean up secret")
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get secret")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func createAnonRole(cr *postgrestv1.Postgrest, ctx context.Context) error {
	log := log.FromContext(ctx)

	// Get connection string from env
	databaseUri, found := os.LookupEnv(postgrestDatabaseUri)
	if !found {
		return errors.New(fmt.Sprintf("Database connection string not specified, environment variable %v is required", postgrestDatabaseUri))
	}
	// Connect to database
	db, err := sql.Open("postgres", databaseUri)
	if err != nil {
		return (err)
	}
	defer db.Close()

	// If anonymous role is defined, check if it exists
	if cr.Spec.AnonRole != "" {
		if cr.Spec.Tables != nil {
			return errors.New("Inconsistent configuration: either specify anonymous role or a list of tables")
		}

		log.Info(fmt.Sprintf("Anonymous role %v specified, its permissions will be used", cr.Spec.AnonRole))
		rows, err := db.Query("SELECT FROM pg_catalog.pg_roles WHERE rolname = $1", cr.Spec.AnonRole)
		if err != nil {
			return (err)
		}
		if rows == nil || !rows.Next() {
			return errors.New("Declared anonymous role does not exist, either manually create it or remove it to use autocreation")
		}
	} else {
		// Create anonymous role
		anonRole := cleanAnonRole(cr.Name)
		rows, err := db.Query("SELECT FROM pg_catalog.pg_roles WHERE rolname = $1", anonRole)
		if err != nil {
			return (err)
		}
		if rows == nil || !rows.Next() {
			_, err = db.Exec(fmt.Sprintf("CREATE ROLE %v LOGIN", anonRole))
			if err != nil {
				return (err)
			}
		}

		// Grant usage on schemas
		for _, schema := range strings.Split(cr.Spec.Schemas, ",") {
			_, err = db.Exec(fmt.Sprintf("GRANT USAGE ON SCHEMA %v TO %v", strings.TrimSpace(schema), anonRole))
			if err != nil {
				return (err)
			}
		}

		var grant string = "SELECT"
		if cr.Spec.Grants != "" {
			grant = cr.Spec.Grants
		}

		// Assign permissions on tables
		for _, table := range cr.Spec.Tables {
			_, err = db.Exec(fmt.Sprintf("GRANT %v ON TABLE %v TO %v", grant, table, anonRole))
			if err != nil {
				return (err)
			}
		}
	}

	return nil
}

func cleanAnonRole(crName string) string {
	cleanDots := strings.Replace(crName, ".", "_", -1)
	cleanSpaces := strings.Replace(cleanDots, " ", "_", -1)
	cleanDashes := strings.Replace(cleanSpaces, "-", "_", -1)
	return strings.ToLower(cleanDashes) + "_postgrest_role"
}

func deleteAnonRole(cr *postgrestv1.Postgrest) error {
	// Get connection string from env
	databaseUri, found := os.LookupEnv(postgrestDatabaseUri)
	if !found {
		return errors.New(fmt.Sprintf("Database connection string not specified, environment variable %v is required", postgrestDatabaseUri))
	}
	// Only delete anonymous role if it was created by the controller
	if cr.Spec.AnonRole == "" {
		// Connect to database
		db, err := sql.Open("postgres", databaseUri)
		if err != nil {
			return (err)
		}
		defer db.Close()

		anonRole := cleanAnonRole(cr.Name)

		_, err = db.Exec(fmt.Sprintf("DROP OWNED BY %v", anonRole))
		if err != nil && err.Error() != fmt.Sprintf("pq: role \"%v\" does not exist", strings.ToLower(anonRole)) {
			return (err)
		}
		_, err = db.Exec(fmt.Sprintf("DROP ROLE IF EXISTS %v", anonRole))
		if err != nil {
			return (err)
		}
	}

	return nil
}

// Will perform the required operations before delete the CR.
func (r *PostgrestReconciler) doFinalizerOperationsForPostgrest(cr *postgrestv1.Postgrest) error {
	err := deleteAnonRole(cr)
	if err != nil {
		return err
	}

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))

	return nil
}

// deploymentForPostgrest returns a Postgrest Deployment object
func (r *PostgrestReconciler) deploymentForPostgrest(
	postgrest *postgrestv1.Postgrest) (*appsv1.Deployment, error) {
	image, found := os.LookupEnv(postgrestImage)
	if !found {
		image = "postgrest/postgrest"
	}
	tag, found := os.LookupEnv(postgrestImageTag)
	if !found {
		tag = "latest"
	}

	ls := labelsForPostgrest(postgrest.Name, tag)
	selectors := selectorsForPostgrest(postgrest.Name)

	optional := false

	envs := []corev1.EnvVar{
		{
			Name: "PGRST_DB_URI",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: formatResourceName(postgrest.Name)},
					Key:                  postgrestDatabaseUri,
					Optional:             &optional,
				},
			},
		},
		{
			Name:  "PGRST_DB_SCHEMA",
			Value: postgrest.Spec.Schemas,
		},
	}

	var anonRole corev1.EnvVar
	anonRole.Name = "PGRST_DB_ANON_ROLE"
	if postgrest.Spec.AnonRole != "" {
		anonRole.Value = postgrest.Spec.AnonRole
	} else {
		anonRole.Value = cleanAnonRole(postgrest.Name)
	}
	envs = append(envs, anonRole)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      formatResourceName(postgrest.Name),
			Namespace: postgrest.Namespace,
			Labels: ls,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectors,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					// TODO(user): Uncomment the following code to configure the nodeAffinity expression
					// according to the platforms which are supported by your solution. It is considered
					// best practice to support multiple architectures. build your manager image using the
					// makefile target docker-buildx. Also, you can use docker manifest inspect <image>
					// to check what are the platforms supported.
					// More info: https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-affinity
					//Affinity: &corev1.Affinity{
					//	NodeAffinity: &corev1.NodeAffinity{
					//		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					//			NodeSelectorTerms: []corev1.NodeSelectorTerm{
					//				{
					//					MatchExpressions: []corev1.NodeSelectorRequirement{
					//						{
					//							Key:      "kubernetes.io/arch",
					//							Operator: "In",
					//							Values:   []string{"amd64", "arm64", "ppc64le", "s390x"},
					//						},
					//						{
					//							Key:      "kubernetes.io/os",
					//							Operator: "In",
					//							Values:   []string{"linux"},
					//						},
					//					},
					//				},
					//			},
					//		},
					//	},
					//},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &[]bool{true}[0],
						// IMPORTANT: seccomProfile was introduced with Kubernetes 1.19
						// If you are looking for to produce solutions to be supported
						// on lower versions you must remove this option.
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Image:           strings.Join([]string{image, tag}, ":"),
						Name:            "postgrest",
						ImagePullPolicy: corev1.PullIfNotPresent,
						// Ensure restrictive context for the container
						// More info: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
						SecurityContext: &corev1.SecurityContext{
							// WARNING: Ensure that the image used defines an UserID in the Dockerfile
							// otherwise the Pod will not run and will fail with "container has runAsNonRoot and image has non-numeric user"".
							// If you want your workloads admitted in namespaces enforced with the restricted mode in OpenShift/OKD vendors
							// then, you MUST ensure that the Dockerfile defines a User ID OR you MUST leave the "RunAsNonRoot" and
							// "RunAsUser" fields empty.
							RunAsNonRoot: &[]bool{true}[0],
							// The Postgrest image does not use a non-zero numeric user as the default user.
							// Due to RunAsNonRoot field being set to true, we need to force the user in the
							// container to a non-zero numeric user. We do this using the RunAsUser field.
							// However, if you are looking to provide solution for K8s vendors like OpenShift
							// be aware that you cannot run under its restricted-v2 SCC if you set this value.
							RunAsUser:                &[]int64{1001}[0],
							AllowPrivilegeEscalation: &[]bool{false}[0],
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{
									"ALL",
								},
							},
						},
						Env:     envs,
						Command: []string{"postgrest"},
					}},
				},
			},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(postgrest, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

func (r *PostgrestReconciler) serviceForPostgrest(postgrest *postgrestv1.Postgrest) (*corev1.Service, error) {
	tag, found := os.LookupEnv(postgrestImageTag)
	if !found {
		tag = "latest"
	}

	var corev1ServiceType corev1.ServiceType
	serviceType, found := os.LookupEnv(postgrestServiceType)
	if found && strings.EqualFold(serviceType, "ClusterIP") {
		corev1ServiceType = corev1.ServiceTypeClusterIP
	} else if !found || serviceType == "" || strings.EqualFold(serviceType, "NodePort") {
		corev1ServiceType = corev1.ServiceTypeNodePort
	} else {
		return nil, errors.New("Invalid service type")
	}

	ls := labelsForPostgrest(postgrest.Name, tag)
	selectors := selectorsForPostgrest(postgrest.Name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      formatResourceName(postgrest.Name),
			Namespace: postgrest.Namespace,
			Labels: ls,
		},
		Spec: corev1.ServiceSpec{
			Selector: selectors,
			Type:     corev1ServiceType,
			Ports: []corev1.ServicePort{{
				Protocol:   corev1.ProtocolTCP,
				Port:       3000,
				TargetPort: intstr.FromInt(3000),
			}},
		},
	}

	if err := ctrl.SetControllerReference(postgrest, service, r.Scheme); err != nil {
		return nil, err
	}

	return service, nil
}

func (r *PostgrestReconciler) secretForPostgrest(postgrest *postgrestv1.Postgrest) (*corev1.Secret, error) {
	databaseUri, found := os.LookupEnv(postgrestDatabaseUri)
	if !found {
		return nil, errors.New(fmt.Sprintf("Database connection string not specified, environment variable %v is required", postgrestDatabaseUri))
	}

	tag, found := os.LookupEnv(postgrestImageTag)
	if !found {
		tag = "latest"
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      formatResourceName(postgrest.Name),
			Namespace: postgrest.Namespace,
			Labels: labelsForPostgrest(postgrest.Name, tag),
		},
		StringData: map[string]string{postgrestDatabaseUri: databaseUri},
	}

	if err := ctrl.SetControllerReference(postgrest, secret, r.Scheme); err != nil {
		return nil, err
	}

	return secret, nil
}

// labelsForPostgrest returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForPostgrest(name string, version string) map[string]string {
	selectors := selectorsForPostgrest(name)
	selectors["app.kubernetes.io/version"] = version
	selectors["app.kubernetes.io/part-of"] = "postgrest"
	return selectors
}

func selectorsForPostgrest(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "Postgrest",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "postgrest-operator",
	}
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *PostgrestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&postgrestv1.Postgrest{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
