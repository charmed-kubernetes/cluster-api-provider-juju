/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"

	infrastructurev1beta1 "github.com/charmed-kubernetes/cluster-api-provider-juju/api/v1beta1"
	kbatch "k8s.io/api/batch/v1"
	kcore "k8s.io/api/core/v1"
	krbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JujuClusterReconciler reconciles a JujuCluster object
type JujuClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujuclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujuclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujuclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the JujuCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *JujuClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	jujuCluster := &infrastructurev1beta1.JujuCluster{}
	if err := r.Get(ctx, req.NamespacedName, jujuCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get JujuCluster")
		return ctrl.Result{}, err
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, jujuCluster.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Could not find the owner yet, this is not an error and will rereconcile when the owner gets set.
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get owner Cluster")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("Cluster owner was nil")
		return ctrl.Result{}, nil
	}

	// Check if a juju controller has been created yet via the job
	job := &kbatch.Job{}
	objectKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-bootstrap",
	}
	if err := r.Get(ctx, objectKey, job); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("juju controller boostrap job not found, bootstrapping now")
			if err := r.createJujuControllerResources(ctx, cluster, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	// Ensure the job has completed
	if job.Status.Succeeded <= 0 {
		log.Info("Waiting for bootstrap job to complete")
		return ctrl.Result{Requeue: true}, nil
	} else {
		log.Info("bootstrap job completed")
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if jujuCluster.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer) {
			controllerutil.AddFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer)
			if err := r.Update(ctx, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer) {
			// our finalizer is present, so lets handle controller resource deletion
			if err := r.deleteJujuControllerResources(ctx, cluster, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer)
			if err := r.Update(ctx, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.JujuCluster{}).
		Complete(r)
}

func (r *JujuClusterReconciler) createJujuControllerResources(ctx context.Context, cluster *clusterv1.Cluster, jujuCluster *infrastructurev1beta1.JujuCluster) error {
	log := log.FromContext(ctx)

	clientSA := &kcore.ServiceAccount{}
	clientSA.Name = jujuCluster.Name + "-juju-client"
	clientSA.Namespace = jujuCluster.Namespace

	clientCRB := &krbac.ClusterRoleBinding{}
	clientCRB.Name = jujuCluster.Name + "-juju-client"
	clientCRB.RoleRef = krbac.RoleRef{
		APIGroup: krbac.GroupName,
		Kind:     "ClusterRole",
		Name:     "cluster-admin",
	}
	clientCRB.Subjects = []krbac.Subject{{
		Kind:      krbac.ServiceAccountKind,
		Name:      clientSA.Name,
		Namespace: jujuCluster.Namespace,
	}}

	clientSecret := &kcore.Secret{}
	clientSecret.Name = jujuCluster.Name + "-juju-client"
	clientSecret.Namespace = jujuCluster.Namespace
	clientSecret.Annotations = make(map[string]string)
	clientSecret.Annotations["kubernetes.io/service-account.name"] = clientSA.Name
	clientSecret.Type = kcore.SecretTypeServiceAccountToken

	cloudName := jujuCluster.Name + "-k8s-cloud"
	containerArgs := fmt.Sprintf("./kubectl config set-cluster cluster --server=https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT_HTTPS --certificate-authority=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt --embed-certs;"+
		"./kubectl config set-credentials user --token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token);"+
		"./kubectl config set-context context --cluster=cluster --user=user;"+
		"./kubectl config use-context context;"+
		"./juju version;"+
		"./juju add-k8s --client %s;"+
		"./juju clouds;"+
		"./juju bootstrap %s", cloudName, cloudName)
	job := &kbatch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        jujuCluster.Name + "-juju-controller-bootstrap",
			Namespace:   jujuCluster.Namespace,
		},
		Spec: kbatch.JobSpec{
			Template: kcore.PodTemplateSpec{
				Spec: kcore.PodSpec{
					RestartPolicy:      kcore.RestartPolicyNever,
					ServiceAccountName: clientSA.Name,
					Containers: []kcore.Container{
						{
							Name: "juju-client",
							Env: []kcore.EnvVar{{
								Name:  "KUBECONFIG",
								Value: "~/.kube/config",
							}},
							Image:   "rocks.canonical.com:443/cdk/capi/juju-client:latest",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{containerArgs},
						},
					},
				},
			},
		},
	}

	objects := []client.Object{
		clientSA,
		clientCRB,
		clientSecret,
		job,
	}

	log.Info("Creating resources necessary for bootstrapping juju controller")
	for _, obj := range objects {
		if err := r.Create(ctx, obj); err != nil {
			log.Error(err, "failed to create object", "kind", obj.GetObjectKind().GroupVersionKind().Kind)
			return err
		}
	}

	return nil
}

func (r *JujuClusterReconciler) deleteJujuControllerResources(ctx context.Context, cluster *clusterv1.Cluster, jujuCluster *infrastructurev1beta1.JujuCluster) error {
	log := log.FromContext(ctx)

	clientSA := &kcore.ServiceAccount{}
	clientSAKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-client",
	}

	clientCRB := &krbac.ClusterRoleBinding{}
	clientCRBKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-client",
	}

	clientSecret := &kcore.Secret{}
	clientSecretKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-client",
	}

	job := &kbatch.Job{}
	jobKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-bootstrap",
	}

	namespace := &kcore.Namespace{}
	namespaceKey := client.ObjectKey{
		Name: fmt.Sprintf("controller-%s", jujuCluster.Name+"-k8s-cloud"),
	}

	type objectKeyPair struct {
		object  client.Object
		key     client.ObjectKey
		options client.DeleteOptions
	}

	bg := metav1.DeletePropagationBackground
	jobOptions := client.DeleteOptions{
		PropagationPolicy: &bg,
	}

	objectKeyPairs := []objectKeyPair{
		{clientSecret, clientSecretKey, client.DeleteOptions{}},
		{clientCRB, clientCRBKey, client.DeleteOptions{}},
		{clientSA, clientSAKey, client.DeleteOptions{}},
		{job, jobKey, jobOptions},
		{namespace, namespaceKey, client.DeleteOptions{}},
	}
	for _, pair := range objectKeyPairs {

		err := r.Get(ctx, pair.key, pair.object)
		name := pair.key.Name
		kind := pair.object.GetObjectKind().GroupVersionKind().Kind
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Info(fmt.Sprintf("%s not found when attempting deletion", name))
			} else {
				log.Error(err, fmt.Sprintf("could not get %s", name))
				return err
			}
		} else {
			log.Info(fmt.Sprintf("Deleting %s %s", kind, name))
			err = r.Delete(ctx, pair.object, &pair.options)
			if err != nil {
				log.Error(err, fmt.Sprintf("%s %s could not be deleted", kind, name))
				return err
			}
		}
	}

	return nil
}
