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

const image = "postgrest/postgrest:v11.2.0"

const postgrestFinalizer = "postgrest.postgrest.digitalhub/finalizer"

// Definitions to manage status conditions
const (
	// typeUnknown = "Unknown"

	// When anon role is being created, before deployment
	typeInitializing = "Initializing"

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

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,resources=postgrests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,resources=postgrests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=postgrest.postgrest.digitalhub,resources=postgrests/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

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
			log.Info("postgrest resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Postgrest")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status are available
	if postgrest.Status.State == "" {
		postgrest.Status.State = typeInitializing
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, "Failed to update Postgrest status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the Postgrest Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, postgrest); err != nil {
			log.Error(err, "Failed to re-fetch Postgrest")
			return ctrl.Result{}, err
		}
	}

	if postgrest.Status.State == typeInitializing {
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
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
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
	}

	// Check if the Postgrest instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isPostgrestMarkedToBeDeleted := postgrest.GetDeletionTimestamp() != nil
	if isPostgrestMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(postgrest, postgrestFinalizer) {
			log.Info("Performing Finalizer Operations for Postgrest before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			// *postgrest.Status.State = typeDegraded

			// if err := r.Status().Update(ctx, postgrest); err != nil {
			// 	log.Error(err, "Failed to update Postgrest status")
			// 	return ctrl.Result{}, err
			// }

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			if err := r.doFinalizerOperationsForPostgrest(postgrest); err != nil {
				log.Error(err, "Finalizer operations failed")
				// return ctrl.Result{Requeue: true}, nil
			}

			// TODO(user): If you add operations to the doFinalizerOperationsForPostgrest method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

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

	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: postgrest.Name, Namespace: postgrest.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {

		// Define a new deployment
		dep, err := r.deploymentForPostgrest(postgrest)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Postgrest")

			// The following implementation will update the status
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

		created := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: postgrest.Name, Namespace: postgrest.Namespace}, created)
		if err == nil {
			log.Info("Creating service")
			service, err := r.serviceForPostgrest(postgrest)
			if err != nil {
				log.Error(err, "Service inizialition failed")
			}
			if err = r.Create(ctx, service); err != nil {
				log.Error(err, "Service creation failed")
				if err := r.Delete(ctx, created); err != nil {
					log.Error(err, "Failed to clean up deployment")
				}
				return ctrl.Result{}, err
			}
			log.Info("Service created")
		}

		postgrest.Status.State = typeRunning

		if err := r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, "Failed to update Postgrest status")
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The following implementation will update the status
	postgrest.Status.State = typeRunning

	if err := r.Status().Update(ctx, postgrest); err != nil {
		log.Error(err, "Failed to update Postgrest status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func createAnonRole(cr *postgrestv1.Postgrest, ctx context.Context) error {
	// Connect to database
	db, err := sql.Open("postgres", cr.Spec.DatabaseUri)
	if err != nil {
		return (err)
	}
	defer db.Close()

	// If anonymous role is defined, check if it exists
	if cr.Spec.AnonRole != "" {
		rows, err := db.Query("SELECT FROM pg_catalog.pg_roles WHERE rolname = $1", cr.Spec.AnonRole)
		if err != nil {
			return (err)
		}
		if rows == nil || !rows.Next() {
			return errors.New("declared anonymous role does not exist")
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

		// Assign permissions on tables
		for _, table := range cr.Spec.Tables {
			_, err = db.Exec(fmt.Sprintf("GRANT ALL ON TABLE %v TO %v", table, anonRole))
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
	// Only delete anonymous role if it was created by the controller
	if cr.Spec.AnonRole == "" {
		// Connect to database
		db, err := sql.Open("postgres", cr.Spec.DatabaseUri)
		if err != nil {
			return (err)
		}
		defer db.Close()

		anonRole := cleanAnonRole(cr.Name)

		_, err = db.Exec(fmt.Sprintf("DROP OWNED BY %v", anonRole))
		if err != nil {
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
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.
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
	ls := labelsForPostgrest(postgrest.Name)

	envs := []corev1.EnvVar{
		{
			Name:  "PGRST_DB_URI",
			Value: postgrest.Spec.DatabaseUri,
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
			Name:      postgrest.Name,
			Namespace: postgrest.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
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
						Image:           image,
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
	ls := labelsForPostgrest(postgrest.Name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      postgrest.Name,
			Namespace: postgrest.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: ls,
			Type:     corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				NodePort:   31247,
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

// labelsForPostgrest returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForPostgrest(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": "Postgrest",
		"app.kubernetes.io/instance": name,
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
