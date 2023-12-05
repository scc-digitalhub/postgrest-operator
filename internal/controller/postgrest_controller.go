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
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/exp/slices"

	_ "github.com/lib/pq"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	operatorv1 "github.com/scc-digitalhub/postgrest-operator/api/v1"
)

const postgrestImage = "POSTGREST_IMAGE"
const postgrestImageTag = "POSTGREST_IMAGE_TAG"
const postgrestServiceType = "POSTGREST_SERVICE_TYPE"

const postgresUriSecretKey = "postgresUri"

const containerLimitsCpu = "POSTGREST_CONTAINER_LIMITS_CPU"
const containerLimitsMemory = "POSTGREST_CONTAINER_LIMITS_MEMORY"
const containerRequestsCpu = "POSTGREST_CONTAINER_REQUESTS_CPU"
const containerRequestsMemory = "POSTGREST_CONTAINER_REQUESTS_MEMORY"

const genericStatusUpdateFailedMessage = "failed to update Postgrest status"

const postgrestFinalizer = "operator.postgrest.org/finalizer"

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

	typeUpdating = "Updating"
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

//+kubebuilder:rbac:groups=operator.postgrest.org,namespace=mynamespace,resources=postgrests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=operator.postgrest.org,namespace=mynamespace,resources=postgrests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=operator.postgrest.org,namespace=mynamespace,resources=postgrests/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,namespace=mynamespace,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,namespace=mynamespace,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *PostgrestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Postgrest instance
	// The purpose is check if the Custom Resource for the Kind Postgrest
	// is applied on the cluster if not we return nil to stop the reconciliation
	postgrest := &operatorv1.Postgrest{}
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
			log.Error(err, genericStatusUpdateFailedMessage)
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	if postgrest.Status.State == typeInitializing {
		log.Info("Creating anonymous role")

		postgresUri, err := r.createConnectionString(postgrest, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		err = createAnonRole(postgrest, ctx, postgresUri)
		if err != nil {
			log.Error(err, "error while handling anonymous role")
			return ctrl.Result{}, err
		}

		postgrest.Status.State = typeDeploying
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, genericStatusUpdateFailedMessage)
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
			secret, err := r.secretForPostgrest(postgrest, ctx)
			if err != nil {
				log.Error(err, "Failed to define new Secret resource for Postgrest")

				postgrest.Status.State = typeError

				if err := r.Status().Update(ctx, postgrest); err != nil {
					log.Error(err, genericStatusUpdateFailedMessage)
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
					log.Error(err, genericStatusUpdateFailedMessage)
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
			log.Error(err, "failed to get deployment")
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
					log.Error(err, genericStatusUpdateFailedMessage)
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
			log.Error(err, "failed to get service")
			// Return error for reconciliation to be re-trigged
			return ctrl.Result{}, err
		}

		postgrest.Status.State = typeRunning
		if err = r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, genericStatusUpdateFailedMessage)
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
			postgresUri, err := r.createConnectionString(postgrest, ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.doFinalizerOperationsForPostgrest(postgrest, postgresUri); err != nil {
				log.Error(err, "Finalizer operations failed")
				return ctrl.Result{Requeue: true}, nil
			}

			// Re-fetch the Postgrest Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, postgrest); err != nil {
				log.Error(err, "failed to re-fetch Postgrest")
				return ctrl.Result{}, err
			}

			postgrest.Status.State = typeDegraded

			if err := r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, genericStatusUpdateFailedMessage)
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Postgrest after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(postgrest, postgrestFinalizer); !ok {
				log.Error(err, "failed to remove finalizer for Postgrest")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, postgrest); err != nil {
				log.Error(err, "failed to remove finalizer for Postgrest")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if postgrest.Status.State == typeRunning {
		dep := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, dep)
		if err != nil {
			log.Error(err, "error while retrieving deployment")
			return ctrl.Result{}, err
		}

		postgresUri, err := r.createConnectionString(postgrest, ctx)
		if err != nil {
			return ctrl.Result{}, err
		}

		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, secret)
		if err != nil {
			return ctrl.Result{}, err
		}

		updated, err := crUpdated(dep, postgrest, postgresUri, string(secret.Data[postgresUriSecretKey]))
		if err != nil {
			log.Error(err, "error while checking for update")
			return ctrl.Result{}, err
		}
		if updated {
			postgrest.Status.State = typeUpdating
			if err = r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, genericStatusUpdateFailedMessage)
				return ctrl.Result{}, err
			}
		}

		// Deployment ready
		if dep.Status.ReadyReplicas > 0 {
			log.Info("Deployment is ready")
			if err = r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, genericStatusUpdateFailedMessage)
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		// Wait up to 15 minutes for deployment to become ready
		if dep.CreationTimestamp.Add(15 * time.Minute).Before(time.Now()) {
			log.Info("Deployment still not ready, setting state to Error")
			postgrest.Status.State = typeError

			if err = r.Status().Update(ctx, postgrest); err != nil {
				log.Error(err, genericStatusUpdateFailedMessage)
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if postgrest.Status.State == typeUpdating {
		log.Info("Updating: deleting previous deployment and anon role")

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

		// Delete auto-generated anonRole
		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: formatResourceName(postgrest.Name), Namespace: postgrest.Namespace}, secret)
		if err != nil {
			return ctrl.Result{}, err
		}
		deleteAutoGeneratedAnonRole(postgrest, string(secret.Data[postgresUriSecretKey]))

		// Delete secret
		if err := r.Delete(ctx, secret); err != nil {
			log.Error(err, "Failed to clean up secret")
		}

		postgrest.Status.State = typeInitializing

		if err := r.Status().Update(ctx, postgrest); err != nil {
			log.Error(err, genericStatusUpdateFailedMessage)
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
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

	// Auto-generated anonymous role is not deleted on error

	return ctrl.Result{}, nil
}

func crUpdated(dep *appsv1.Deployment, cr *operatorv1.Postgrest, newDatabaseUri string, oldDatabaseUri string) (bool, error) {
	if newDatabaseUri != oldDatabaseUri {
		return true, nil
	}

	previousAnonRole := ""
	previousSchema := ""
	for _, env := range dep.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "PGRST_DB_ANON_ROLE" {
			previousAnonRole = env.Value
			if (cr.Spec.AnonRole != "" && previousAnonRole != cr.Spec.AnonRole) || (cr.Spec.AnonRole == "" && previousAnonRole != cleanAnonRole(cr.Name)) {
				return true, nil
			}
		}
		if env.Name == "PGRST_DB_SCHEMAS" {
			previousSchema = env.Value
			if previousSchema != cr.Spec.Schema {
				return true, nil
			}
		}
	}

	if previousAnonRole == cleanAnonRole(cr.Name) {
		// Connect to database
		db, err := sql.Open("postgres", newDatabaseUri)
		if err != nil {
			return false, err
		}
		defer db.Close()

		rows, err := db.Query("SELECT table_schema, table_name, privilege_type FROM information_schema.role_table_grants WHERE grantee = $1", previousAnonRole)
		if err != nil {
			return false, err
		}
		defer rows.Close()

		cleanedNewGrants := strings.ReplaceAll(cr.Spec.Grants, " ", "")
		newGrants := strings.Split(strings.ToUpper(cleanedNewGrants), ",")
		if len(newGrants) == 0 || (len(newGrants) == 1 && newGrants[0] == "") {
			newGrants = []string{"SELECT"}
		}

		oldGrants := []string{}
		oldTables := []string{}

		schema := getSchema(cr)

		for rows.Next() {
			var tableSchema string
			var tableName string
			var privilegeType string
			rows.Scan(&tableSchema, &tableName, &privilegeType)

			// Grants
			if !slices.Contains(newGrants, privilegeType) {
				return true, nil
			}

			if !slices.Contains(oldGrants, privilegeType) {
				oldGrants = append(oldGrants, privilegeType)
			}

			// Tables
			if tableSchema != schema {
				return true, nil
			}

			if !slices.Contains(cr.Spec.Tables, tableName) {
				return true, nil
			}

			if !slices.Contains(oldTables, tableName) {
				oldTables = append(oldTables, tableName)
			}
		}

		for _, grant := range newGrants {
			if !slices.Contains(oldGrants, grant) {
				return true, nil
			}
		}

		for _, table := range cr.Spec.Tables {
			if !slices.Contains(oldTables, table) {
				return true, nil
			}
		}
	}

	return false, nil
}

func createAnonRole(cr *operatorv1.Postgrest, ctx context.Context, databaseUri string) error {
	log := log.FromContext(ctx)

	// Connect to database
	db, err := sql.Open("postgres", databaseUri)
	if err != nil {
		return (err)
	}
	defer db.Close()

	// If anonymous role is defined, check if it exists
	if cr.Spec.AnonRole != "" {
		if cr.Spec.Tables != nil {
			return errors.New("inconsistent configuration: either specify anonymous role or a list of tables")
		}

		log.Info(fmt.Sprintf("Anonymous role %v specified, its permissions will be used", cr.Spec.AnonRole))
		rows, err := db.Query("SELECT FROM pg_catalog.pg_roles WHERE rolname = $1", cr.Spec.AnonRole)
		if err != nil {
			return (err)
		}
		if rows == nil || !rows.Next() {
			return errors.New("declared anonymous role does not exist, either manually create it or remove it to trigger auto-creation")
		}
	} else {
		if cr.Spec.Tables == nil || len(cr.Spec.Tables) == 0 {
			return errors.New("the list of tables to expose is empty")
		}

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

		// Grant usage on schema
		schema := getSchema(cr)
		_, err = db.Exec(fmt.Sprintf("GRANT USAGE ON SCHEMA %v TO %v", schema, anonRole))
		if err != nil {
			return (err)
		}

		// Assign permissions on tables
		var grant string = "SELECT"
		if cr.Spec.Grants != "" {
			grant = cr.Spec.Grants
		}

		for _, table := range cr.Spec.Tables {
			_, err = db.Exec(fmt.Sprintf("GRANT %v ON TABLE %v.%v TO %v", grant, schema, table, anonRole))
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

func deleteAutoGeneratedAnonRole(cr *operatorv1.Postgrest, databaseUri string) error {
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

	return nil
}

func deleteAnonRole(cr *operatorv1.Postgrest, postgresUri string) error {
	// Only delete anonymous role if it was created by the controller
	if cr.Spec.AnonRole == "" {
		return deleteAutoGeneratedAnonRole(cr, postgresUri)
	}

	return nil
}

// Will perform the required operations before delete the CR.
func (r *PostgrestReconciler) doFinalizerOperationsForPostgrest(cr *operatorv1.Postgrest, postgresUri string) error {
	err := deleteAnonRole(cr, postgresUri)
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
	postgrest *operatorv1.Postgrest) (*appsv1.Deployment, error) {
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

	envs := []corev1.EnvVar{
		{
			Name: "PGRST_DB_URI",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: formatResourceName(postgrest.Name)},
					Key:                  postgresUriSecretKey,
					Optional:             &[]bool{false}[0],
				},
			},
		},
		{
			Name:  "PGRST_DB_SCHEMAS",
			Value: getSchema(postgrest),
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
			Labels:    ls,
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
						Resources: corev1.ResourceRequirements{
							Limits:   getLimits(),
							Requests: getRequests(),
						},
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

func (r *PostgrestReconciler) serviceForPostgrest(postgrest *operatorv1.Postgrest) (*corev1.Service, error) {
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
		return nil, errors.New("invalid service type")
	}

	ls := labelsForPostgrest(postgrest.Name, tag)
	selectors := selectorsForPostgrest(postgrest.Name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      formatResourceName(postgrest.Name),
			Namespace: postgrest.Namespace,
			Labels:    ls,
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

func (r *PostgrestReconciler) createConnectionString(postgrest *operatorv1.Postgrest, ctx context.Context) (string, error) {
	host := postgrest.Spec.Connection.Host
	if host == "" {
		return "", fmt.Errorf("postgres host missing from spec")
	}

	database := postgrest.Spec.Connection.Database
	if database == "" {
		return "", fmt.Errorf("postgres database missing from spec")
	}

	user := postgrest.Spec.Connection.User
	password := postgrest.Spec.Connection.Password
	secretName := postgrest.Spec.Connection.SecretName

	// check that there is either password or secretName
	if secretName != "" {
		if password != "" || user != "" {
			return "", fmt.Errorf("either specify user and password or secretName")
		}

		//read secret
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: postgrest.Namespace}, secret)
		if err != nil {
			return "", err
		}

		//check if secret contains POSTGRES_URL
		postgresUrlFromSecret := secret.Data["POSTGRES_URL"]
		if postgresUrlFromSecret != nil {
			//compare with host and database
			u, err := url.Parse(string(postgresUrlFromSecret))
			if err != nil {
				return "", fmt.Errorf("secret contains invalid POSTGRES_URL")
			}
			h, _, _ := net.SplitHostPort(u.Host)
			if h != host || u.Path[1:] != database {
				return "", fmt.Errorf("host or database in POSTGRES_URL does not match CR spec")
			}
			return string(postgresUrlFromSecret), nil
		} else {
			//check that secret contains USER and PASSWORD
			userFromSecret := secret.Data["USER"]
			passwordFromSecret := secret.Data["PASSWORD"]
			if userFromSecret == nil || passwordFromSecret == nil {
				return "", fmt.Errorf("secret must contain USER and PASSWORD")
			}

			user = string(userFromSecret)
			password = string(passwordFromSecret)
		}
	} else if password == "" || user == "" {
		return "", fmt.Errorf("specify both user and password")
	}

	port := postgrest.Spec.Connection.Port
	queryParams := postgrest.Spec.Connection.ExtraParams

	if port != 0 {
		host = fmt.Sprintf("%v:%v", host, port)
	}

	postgresUri := fmt.Sprintf("postgresql://%v:%v@%v/%v", user, password, host, database)

	if queryParams != "" {
		postgresUri += fmt.Sprintf("?%v", queryParams)
	}

	return postgresUri, nil
}

func (r *PostgrestReconciler) secretForPostgrest(postgrest *operatorv1.Postgrest, ctx context.Context) (*corev1.Secret, error) {
	tag, found := os.LookupEnv(postgrestImageTag)
	if !found {
		tag = "latest"
	}

	postgresUri, err := r.createConnectionString(postgrest, ctx)
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      formatResourceName(postgrest.Name),
			Namespace: postgrest.Namespace,
			Labels:    labelsForPostgrest(postgrest.Name, tag),
		},
		StringData: map[string]string{postgresUriSecretKey: postgresUri},
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
		For(&operatorv1.Postgrest{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func getSchema(cr *operatorv1.Postgrest) string {
	schema := cr.Spec.Schema
	if schema == "" {
		schema = "public"
	}
	return schema
}

func getLimits() corev1.ResourceList {
	limits := corev1.ResourceList{}

	limitsCpu, found := os.LookupEnv(containerLimitsCpu)
	if found {
		limits[corev1.ResourceCPU] = resource.MustParse(limitsCpu)
	}
	limitsMemory, found := os.LookupEnv(containerLimitsMemory)
	if found {
		limits[corev1.ResourceMemory] = resource.MustParse(limitsMemory)
	}

	return limits
}

func getRequests() corev1.ResourceList {
	requests := corev1.ResourceList{}

	requestsCpu, found := os.LookupEnv(containerRequestsCpu)
	if found {
		requests[corev1.ResourceCPU] = resource.MustParse(requestsCpu)
	}
	requestsMemory, found := os.LookupEnv(containerRequestsMemory)
	if found {
		requests[corev1.ResourceMemory] = resource.MustParse(requestsMemory)
	}

	return requests
}
