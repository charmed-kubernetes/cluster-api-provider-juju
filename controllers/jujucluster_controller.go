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
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	infrastructurev1beta1 "github.com/charmed-kubernetes/cluster-api-provider-juju/api/v1beta1"
	"github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/juju/juju/cloud"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/life"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
	kbatch "k8s.io/api/batch/v1"
	kcore "k8s.io/api/core/v1"
	krbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JujuConfig is used for parsing necessary config values from juju-client output
type JujuConfig struct {
	Details struct {
		APIEndpoints []string `yaml:"api-endpoints"`
		CACert       string   `yaml:"ca-cert"`
	}
	Account struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	}
}

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
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
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
			log.Info("waiting for cluster owner to be found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get owner Cluster")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("waiting for cluster owner to be non-nil")
		return ctrl.Result{}, nil
	}

	// Check if a secret containing cloud user data has been created yet
	cloudSecret, err := r.getCloudSecret(ctx, jujuCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cloudSecret == nil {
		log.Info(fmt.Sprintf("secret %s not found in namespace %s, please create it", jujuCluster.Name+"-cloud-secret", jujuCluster.Namespace))
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if a juju controller has been created yet via the job
	bootstrapJob, err := r.getBootstrapJob(ctx, jujuCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if bootstrapJob == nil {
		log.Info("juju controller boostrap job not found, bootstrapping now")
		if err := r.createJujuControllerResources(ctx, cluster, jujuCluster); err != nil {
			return ctrl.Result{}, err
		}
		// Give it some time to create the job
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	// Ensure the job has completed
	if bootstrapJob.Status.Succeeded <= 0 {
		log.Info("waiting for bootstrap job to complete")
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if juju config map has been created yet
	jujuConfigMap, err := r.getJujuConfigMap(ctx, jujuCluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if jujuConfigMap == nil {
		log.Info("juju controller config not found, creating it now")
		if err := r.createJujuConfigMap(ctx, cluster, jujuCluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	connectorConfig := juju.Configuration{
		ControllerAddresses: strings.Split(jujuConfigMap.Data["api_endpoints"], ","),
		Username:            jujuConfigMap.Data["user"],
		Password:            jujuConfigMap.Data["password"],
		CACert:              jujuConfigMap.Data["ca_cert"],
	}

	jujuClient, err := juju.NewClient(connectorConfig)
	if err != nil {
		log.Error(err, "failed to create juju client")
		return ctrl.Result{}, err
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
			log.Info("added finalizer")
			return ctrl.Result{}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer) {
			// our finalizer is present, so lets handle controller resource deletion
			modelExists, err := jujuClient.Models.ModelExists(ctx, jujuCluster.Name)
			if err != nil {
				log.Error(err, "failed to query existing models")
			}
			if modelExists {
				model, err := jujuClient.Models.GetModelByName(ctx, jujuCluster.Name)
				if err != nil {
					log.Error(err, "failed to get model info")
					return ctrl.Result{}, err
				}
				if model != nil {
					if model.Life == life.Alive {
						if model.Status.Status != "destroying" {
							modelUUID, err := jujuClient.Models.GetModelUUID(ctx, jujuCluster.Name)
							if err != nil {
								log.Error(err, "failed to retrieve modelUUID when attempting to destroy model")
								return ctrl.Result{}, err
							}

							if modelUUID != "" {

								easyRSAExistsInput := juju.ApplicationExistsInput{
									ModelUUID:       modelUUID,
									ApplicationName: "easyrsa",
								}
								easyRSAExists, err := jujuClient.Applications.ApplicationExists(ctx, easyRSAExistsInput)
								if err != nil {
									log.Error(err, "failed to query existing applications")
									return ctrl.Result{}, err
								}
								if easyRSAExists {
									log.Info("destroying easyrsa")
									destroyEasyRSAInput := juju.DestroyApplicationInput{
										ApplicationName: "easyrsa",
										ModelUUID:       modelUUID,
									}
									if err := jujuClient.Applications.DestroyApplication(ctx, &destroyEasyRSAInput); err != nil {
										log.Error(err, "failed to destroy easyrsa")
										return ctrl.Result{}, err
									}
									log.Info("request to destroy easyrsa succeeded, requeueing")
									return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
								}

								loadBalancerExistsInput := juju.ApplicationExistsInput{
									ModelUUID:       modelUUID,
									ApplicationName: "kubeapi-load-balancer",
								}
								loadBalancerExists, err := jujuClient.Applications.ApplicationExists(ctx, loadBalancerExistsInput)
								if err != nil {
									log.Error(err, "failed to query existing applications")
									return ctrl.Result{}, err
								}
								if loadBalancerExists {
									log.Info("destroying kubeapi-load-balancer")
									destroyLoadBalancerInput := juju.DestroyApplicationInput{
										ApplicationName: "kubeapi-load-balancer",
										ModelUUID:       modelUUID,
									}
									if err := jujuClient.Applications.DestroyApplication(ctx, &destroyLoadBalancerInput); err != nil {
										log.Error(err, "failed to destroy kubeapi-load-balancer")
										return ctrl.Result{}, err
									}
									log.Info("request to destroy kubeapi-load-balancer succeeded, requeueing")
									return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
								}

								log.Info("destroying model")
								destroyModelInput := juju.DestroyModelInput{
									UUID: modelUUID,
								}
								if err := jujuClient.Models.DestroyModel(ctx, destroyModelInput); err != nil {
									log.Error(err, "failed to destroy model")
									return ctrl.Result{}, err
								}
								log.Info("request to destroy model succeeded, requeueing")
								return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
							}
						} else {
							log.Info("model is being destroyed, requeueing")
							return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
						}
					}
				}
			}

			log.Info("destroying controller")
			destroyControllerInput := juju.DestroyControllerInput{
				DestroyModels:  true,
				DestroyStorage: true,
				Force:          false,
				MaxWait:        10 * time.Minute,
				ModelTimeout:   30 * time.Minute,
			}
			if err := jujuClient.Controller.DestroyController(ctx, destroyControllerInput); err != nil {
				log.Error(err, "failed to destroy controller")
				return ctrl.Result{}, err
			}

			log.Info("deleting juju controller resources")
			if err := r.deleteJujuControllerResources(ctx, cluster, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			log.Info("removing finalizer")
			controllerutil.RemoveFinalizer(jujuCluster, infrastructurev1beta1.JujuClusterFinalizer)
			if err := r.Update(ctx, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		log.Info("stopping reconciliation")
		return ctrl.Result{}, nil
	}

	cloudExistsInput := juju.CloudExistsInput{
		Name: jujuCluster.Name,
	}
	cloudExists, err := jujuClient.Clouds.CloudExists(ctx, cloudExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing clouds")
		return ctrl.Result{}, err
	}

	if !cloudExists {
		log.Info("cloud not found, creating it now")
		err = createCloud(ctx, jujuCluster, jujuClient)
		if err != nil {
			log.Error(err, "failed to add cloud")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check if credential has been created yet
	credExistsInput := juju.CredentialExistsInput{
		CredentialName: jujuCluster.Name,
		CloudName:      jujuCluster.Name,
	}
	credentialExists, err := jujuClient.Credentials.CredentialExists(ctx, credExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing credentials")
		return ctrl.Result{}, err
	}
	if !credentialExists {
		log.Info("credential not found, creating it now")
		err = createCredential(ctx, cloudSecret, jujuCluster, jujuClient)
		if err != nil {
			log.Error(err, "failed to add credential")
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check if model has been created yet
	modelExists, err := jujuClient.Models.ModelExists(ctx, jujuCluster.Name)
	if err != nil {
		log.Error(err, "failed to query existing models")
		return ctrl.Result{}, err
	}
	if !modelExists {
		log.Info("model not found, creating it now")
		response, err := createModel(ctx, jujuCluster, jujuClient)
		if err != nil {
			log.Error(err, "Error creating model")
			return ctrl.Result{}, err
		}

		log.Info("created model", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	modelUUID, err := jujuClient.Models.GetModelUUID(ctx, jujuCluster.Name)
	if err != nil {
		log.Error(err, "failed to retrieve modelUUID")
		return ctrl.Result{}, err
	}

	easyRSAExistsInput := juju.ApplicationExistsInput{
		ModelUUID:       modelUUID,
		ApplicationName: "easyrsa",
	}
	easyRSAExists, err := jujuClient.Applications.ApplicationExists(ctx, easyRSAExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing applications")
		return ctrl.Result{}, err
	}
	if !easyRSAExists {
		log.Info("easyrsa application not found, creating it now")
		response, err := createEasyRSA(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "Error creating easyrsa")
			return ctrl.Result{}, err
		}
		log.Info("created easyrsa", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	loadBalancerExistsInput := juju.ApplicationExistsInput{
		ModelUUID:       modelUUID,
		ApplicationName: "kubeapi-load-balancer",
	}
	loadBalancerExists, err := jujuClient.Applications.ApplicationExists(ctx, loadBalancerExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing applications")
		return ctrl.Result{}, err
	}
	if !loadBalancerExists {
		log.Info("kubeapi-load-balancer application not found, creating it now")
		response, err := createKubeAPILoadBalancer(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "Error creating kubeapi-load-balancer")
			return ctrl.Result{}, err
		}
		log.Info("created kubeapi-load-balancer", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	controlPlaneExistsInput := juju.ApplicationExistsInput{
		ModelUUID:       modelUUID,
		ApplicationName: "kubernetes-control-plane",
	}
	controlPlaneExists, err := jujuClient.Applications.ApplicationExists(ctx, controlPlaneExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing applications")
		return ctrl.Result{}, err
	}
	if !controlPlaneExists {
		log.Info("control plane application not found, creating it now")
		response, err := createControlPlane(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "Error creating control plane application")
			return ctrl.Result{}, err
		}
		log.Info("created control plane application", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	workerExistsInput := juju.ApplicationExistsInput{
		ModelUUID:       modelUUID,
		ApplicationName: "kubernetes-worker",
	}
	workerExists, err := jujuClient.Applications.ApplicationExists(ctx, workerExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing applications")
		return ctrl.Result{}, err
	}
	if !workerExists {
		log.Info("worker application not found, creating it now")
		response, err := createWorker(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "Error creating worker application")
			return ctrl.Result{}, err
		}
		log.Info("created worker application", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	etcdExistsInput := juju.ApplicationExistsInput{
		ModelUUID:       modelUUID,
		ApplicationName: "etcd",
	}
	etcdExists, err := jujuClient.Applications.ApplicationExists(ctx, etcdExistsInput)
	if err != nil {
		log.Error(err, "failed to query existing applications")
		return ctrl.Result{}, err
	}
	if !etcdExists {
		log.Info("etcd application not found, creating it now")
		response, err := createEtcd(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "Error creating etcd application")
			return ctrl.Result{}, err
		}
		log.Info("created etcd application", "response", response)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	err = createIntegrationsIfNeeded(ctx, jujuCluster, jujuClient, modelUUID)
	if err != nil {
		log.Error(err, "error creating integrations")
		return ctrl.Result{}, err
	}

	readEasyRSAInput := juju.ReadApplicationInput{
		ModelUUID:       modelUUID,
		ApplicationName: "easyrsa",
	}
	easyRSAActiveIdle, err := jujuClient.Applications.AreApplicationUnitsActiveIdle(ctx, readEasyRSAInput)
	if err != nil {
		log.Error(err, "Error querying easyrsa unit status")
		return ctrl.Result{}, err
	}
	if !easyRSAActiveIdle {
		log.Info("easyrsa units are not yet active/idle, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	readLoadBalancerInput := juju.ReadApplicationInput{
		ModelUUID:       modelUUID,
		ApplicationName: "kubeapi-load-balancer",
	}
	loadBalancerActiveIdle, err := jujuClient.Applications.AreApplicationUnitsActiveIdle(ctx, readLoadBalancerInput)
	if err != nil {
		log.Error(err, "Error querying kubeapi-load-balancer unit status")
		return ctrl.Result{}, err
	}
	if !loadBalancerActiveIdle {
		log.Info("kubeapi-load-balancer units are not yet active/idle, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if jujuCluster.Spec.ControlPlaneEndpoint.Host == "" {
		log.Info("cluster control plane endpoint host was empty, setting it now")
		address, err := getLoadBalancerAddresss(ctx, jujuCluster, jujuClient, modelUUID)
		if err != nil {
			log.Error(err, "error retreiving load balancer address")
			return ctrl.Result{}, err
		}
		if address == nil {
			log.Error(err, "load balancer address was nil")
			return ctrl.Result{}, fmt.Errorf("load balancer address is nil")
		}
		jujuCluster.Spec.ControlPlaneEndpoint.Host = *address
		jujuCluster.Spec.ControlPlaneEndpoint.Port = 443
		if err := r.Update(ctx, jujuCluster); err != nil {
			log.Error(err, "error updating control plane endpoint")
			return ctrl.Result{}, err
		}
		log.Info("successfully updated JujuCluster", "Spec.ControlPlaneEndpoint", jujuCluster.Spec.ControlPlaneEndpoint)
		return ctrl.Result{}, nil
	}

	if !jujuCluster.Status.Ready {
		helper, err := patch.NewHelper(jujuCluster, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}
		log.Info("patching cluster ready status to true")
		jujuCluster.Status.Ready = true
		if err := helper.Patch(ctx, jujuCluster); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "couldn't patch cluster %q", jujuCluster.Name)
		}
	}

	log.Info("stopping reconciliation")
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
		"./juju add-k8s --client %s;"+
		"./juju bootstrap %s --config controller-service-type=%s;"+
		"./juju add-user capi-juju;"+
		"./juju grant capi-juju superuser;"+
		"./juju show-controller --show-password", cloudName, cloudName, jujuCluster.Spec.ControllerServiceType)
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

	log.Info("creating resources necessary for bootstrapping juju controller")
	for _, obj := range objects {
		if err := r.Create(ctx, obj); err != nil {
			log.Error(err, "failed to create object")
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
			log.Info(fmt.Sprintf("deleting %s %s", kind, name))
			err = r.Delete(ctx, pair.object, &pair.options)
			if err != nil {
				log.Error(err, fmt.Sprintf("%s %s could not be deleted", kind, name))
				return err
			}
		}
	}

	return nil
}

func (r *JujuClusterReconciler) getClientLogs(ctx context.Context, cluster *clusterv1.Cluster, jujuCluster *infrastructurev1beta1.JujuCluster) (string, error) {
	log := log.FromContext(ctx)

	// N.B controller runtime client does not support getting subresources like logs from pods
	// have to use client-go

	// Try using the in-cluster config,
	// if theres an error (such as when running outside the cluster), fall back to kubeconfig
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := (*flag.Lookup("kubeconfig")).Value.(flag.Getter).Get().(string)
		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			panic(err.Error())
		}
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	job, err := clientset.BatchV1().Jobs(jujuCluster.Namespace).Get(context.TODO(), jujuCluster.Name+"-juju-controller-bootstrap", metav1.GetOptions{})
	if err != nil {
		log.Error(err, fmt.Sprintf("error getting job %s in namespace %s", jujuCluster.Name+"-juju-controller-bootstrap", jujuCluster.Namespace))
		return "", err
	}

	pods, err := clientset.CoreV1().Pods(jujuCluster.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(job.Spec.Selector)})
	if err != nil {
		log.Error(err, fmt.Sprintf("error listing pods %s in namespace", jujuCluster.Namespace))
		return "", err
	}

	logs := ""
	for _, pod := range pods.Items {
		req := clientset.CoreV1().Pods(jujuCluster.Namespace).GetLogs(pod.Name, &kcore.PodLogOptions{})
		podLogs, err := req.Stream(ctx)
		if err != nil {
			log.Error(err, "error opening stream")
			return "", err
		}
		defer podLogs.Close()
		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, podLogs)
		if err != nil {
			log.Error(err, "error in copying information from logs to buffer")
			return "", err
		}

		logs = logs + buf.String()
	}

	return logs, nil
}

func getJujuConfigFromLogs(ctx context.Context, logs string, cloud string) (JujuConfig, error) {
	log := log.FromContext(ctx)

	split := strings.Split(logs, fmt.Sprintf("%s:\n", cloud))
	yam := split[1]
	config := JujuConfig{}
	err := yaml.Unmarshal([]byte(yam), &config)
	if err != nil {
		log.Error(err, "error unmarshalling YAML data into config struct")
		return JujuConfig{}, err
	}

	return config, nil

}

func (r *JujuClusterReconciler) createJujuConfigMap(ctx context.Context, cluster *clusterv1.Cluster, jujuCluster *infrastructurev1beta1.JujuCluster) error {
	log := log.FromContext(ctx)

	logs, err := r.getClientLogs(ctx, cluster, jujuCluster)
	if err != nil {
		log.Error(err, "error getting juju client pod logs")
		return err
	}

	jujuConfig, err := getJujuConfigFromLogs(ctx, logs, jujuCluster.Name+"-k8s-cloud")
	if err != nil {
		log.Error(err, "error getting juju config from client pod logs")
		return err
	}

	jujuConfigMap := &kcore.ConfigMap{}
	jujuConfigMap.Name = jujuCluster.Name + "-juju-controller-config"
	jujuConfigMap.Namespace = jujuCluster.Namespace
	jujuConfigMap.Data = make(map[string]string)
	jujuConfigMap.Data["api_endpoints"] = strings.Join(jujuConfig.Details.APIEndpoints[:], ",")
	jujuConfigMap.Data["ca_cert"] = jujuConfig.Details.CACert
	jujuConfigMap.Data["user"] = jujuConfig.Account.User
	jujuConfigMap.Data["password"] = jujuConfig.Account.Password

	if err := controllerutil.SetControllerReference(jujuCluster, jujuConfigMap, r.Scheme); err != nil {
		log.Error(err, "failed to set controller reference on config map")
		return err
	}

	if err := r.Create(ctx, jujuConfigMap); err != nil {
		log.Error(err, "failed to create juju controller config map")
		return err
	}

	return nil
}

func (r *JujuClusterReconciler) getCloudSecret(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster) (*kcore.Secret, error) {
	// Check if a secret containing cloud user data has been created yet
	cloudSecret := &kcore.Secret{}
	objectKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-cloud-secret",
	}
	if err := r.Get(ctx, objectKey, cloudSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}

	return cloudSecret, nil
}

func (r *JujuClusterReconciler) getBootstrapJob(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster) (*kbatch.Job, error) {
	job := &kbatch.Job{}
	objectKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-bootstrap",
	}
	if err := r.Get(ctx, objectKey, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return job, nil
}

func (r *JujuClusterReconciler) getJujuConfigMap(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster) (*kcore.ConfigMap, error) {
	jujuConfigMap := &kcore.ConfigMap{}
	objectKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-config",
	}
	if err := r.Get(ctx, objectKey, jujuConfigMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return jujuConfigMap, nil
}

func createCloud(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client) error {
	vsphereRegion := cloud.Region{
		Name:     "Boston",
		Endpoint: jujuCluster.Spec.CloudEndpoint,
	}

	vsphereCloud := cloud.Cloud{
		Name:      jujuCluster.Name,
		Type:      "vsphere",
		Endpoint:  jujuCluster.Spec.CloudEndpoint,
		Regions:   []cloud.Region{vsphereRegion},
		AuthTypes: []cloud.AuthType{"userpass"},
	}

	// Force must be true when adding a non-k8s cloud to a in-k8s juju controller
	addCloudInput := juju.AddCloudInput{
		Cloud: vsphereCloud,
		Force: true,
	}
	return client.Clouds.AddCloud(ctx, addCloudInput)
}

func createCredential(ctx context.Context, cloudSecret *kcore.Secret, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client) error {
	username := string(cloudSecret.Data["username"][:])
	password := string(cloudSecret.Data["password"][:])
	vmfolder := string(cloudSecret.Data["vmfolder"][:])
	credential := cloud.NewCredential("userpass", map[string]string{
		"user":     username,
		"password": password,
		"vmfolder": vmfolder,
	})
	addCredInput := juju.AddCredentialInput{
		Credential:     credential,
		CredentialName: jujuCluster.Name,
		CloudName:      jujuCluster.Name,
	}
	return client.Credentials.AddCredential(ctx, addCredInput)
}

func createModel(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client) (*juju.CreateModelResponse, error) {
	config := make(map[string]interface{})
	config["juju-http-proxy"] = "http://squid.internal:3128"
	config["apt-http-proxy"] = "http://squid.internal:3128"
	config["snap-http-proxy"] = "http://squid.internal:3128"
	config["juju-https-proxy"] = "http://squid.internal:3128"
	config["apt-https-proxy"] = "http://squid.internal:3128"
	config["snap-https-proxy"] = "http://squid.internal:3128"
	config["apt-no-proxy"] = "localhost,127.0.0.1,ppa.launchpad.net,launchpad.net"
	config["juju-no-proxy"] = "localhost,127.0.0.1,0.0.0.0,ppa.launchpad.net,launchpad.net,10.0.8.0/24,10.246.154.0/24"
	config["logging-config"] = "<root>=DEBUG"
	config["datastore"] = "vsanDatastore"
	config["primary-network"] = "VLAN_2764"
	createModelInput := juju.CreateModelInput{
		Name:           jujuCluster.Name,
		Cloud:          jujuCluster.Name,
		CloudRegion:    "Boston",
		CredentialName: jujuCluster.Name,
		Config:         config,
		Constraints:    constraints.Value{},
	}

	response, err := client.Models.CreateModel(ctx, createModelInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createEasyRSA(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*juju.CreateApplicationResponse, error) {
	log := log.FromContext(ctx)
	cons, err := constraints.Parse("cores=1", "mem=4G", "root-disk=16G")
	if err != nil {
		log.Error(err, "error creating machine constraints")
		return nil, nil
	}
	createApplicationInput := juju.CreateApplicationInput{
		ApplicationName: "easyrsa",
		ModelUUID:       modelUUID,
		CharmName:       "easyrsa",
		CharmChannel:    "1.26/stable",
		CharmBase:       "ubuntu@22.04",
		CharmRevision:   32,
		Units:           1,
		Trust:           true,
		Constraints:     cons,
	}

	response, err := client.Applications.CreateApplication(ctx, &createApplicationInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createKubeAPILoadBalancer(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*juju.CreateApplicationResponse, error) {
	log := log.FromContext(ctx)
	cons, err := constraints.Parse("cores=1", "mem=4G", "root-disk=16G")
	if err != nil {
		log.Error(err, "error creating machine constraints")
		return nil, nil
	}
	createApplicationInput := juju.CreateApplicationInput{
		ApplicationName: "kubeapi-load-balancer",
		ModelUUID:       modelUUID,
		CharmName:       "kubeapi-load-balancer",
		CharmChannel:    "1.26/stable",
		CharmBase:       "ubuntu@22.04",
		CharmRevision:   53,
		Units:           1,
		Trust:           true,
		Constraints:     cons,
	}

	response, err := client.Applications.CreateApplication(ctx, &createApplicationInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func getLoadBalancerAddresss(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*string, error) {
	readLoadBalancerInput := juju.ReadApplicationInput{
		ModelUUID:       modelUUID,
		ApplicationName: "kubeapi-load-balancer",
	}
	address, err := client.Applications.GetLeaderAddress(ctx, readLoadBalancerInput)
	if err != nil {
		return nil, err
	}

	return address, nil
}

func createControlPlane(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*juju.CreateApplicationResponse, error) {
	createApplicationInput := juju.CreateApplicationInput{
		ApplicationName: "kubernetes-control-plane",
		ModelUUID:       modelUUID,
		CharmName:       "kubernetes-control-plane",
		CharmChannel:    "1.26/stable",
		CharmBase:       "ubuntu@22.04",
		CharmRevision:   231,
		Units:           0,
		Trust:           true,
	}

	response, err := client.Applications.CreateApplication(ctx, &createApplicationInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createWorker(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*juju.CreateApplicationResponse, error) {
	createApplicationInput := juju.CreateApplicationInput{
		ApplicationName: "kubernetes-worker",
		ModelUUID:       modelUUID,
		CharmName:       "kubernetes-worker",
		CharmChannel:    "1.26/stable",
		CharmBase:       "ubuntu@22.04",
		CharmRevision:   87,
		Units:           0,
		Trust:           true,
	}

	response, err := client.Applications.CreateApplication(ctx, &createApplicationInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createEtcd(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*juju.CreateApplicationResponse, error) {
	createApplicationInput := juju.CreateApplicationInput{
		ApplicationName: "etcd",
		ModelUUID:       modelUUID,
		CharmName:       "etcd",
		CharmChannel:    "1.26/stable",
		CharmBase:       "ubuntu@22.04",
		CharmRevision:   724,
		Units:           0,
		Trust:           true,
	}

	response, err := client.Applications.CreateApplication(ctx, &createApplicationInput)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func createIntegrationsIfNeeded(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) error {
	log := log.FromContext(ctx)
	integrationInputs := []juju.IntegrationInput{
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubeapi-load-balancer:certificates", "easyrsa:client"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-control-plane:certificates", "easyrsa:client"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-worker:certificates", "easyrsa:client"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"etcd:certificates", "easyrsa:client"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-control-plane:etcd", "etcd:db"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-control-plane:kube-control", "kubernetes-worker:kube-control"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-control-plane:loadbalancer-external", "kubeapi-load-balancer:lb-consumers"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"kubernetes-control-plane:loadbalancer-internal", "kubeapi-load-balancer:lb-consumers"},
		},
	}

	for _, integrationInput := range integrationInputs {
		integrationExists, err := client.Integrations.IntegrationExists(ctx, &integrationInput)
		if err != nil {
			log.Error(err, fmt.Sprintf("Error querying existing integrations for integration: %+v", integrationInput))
			return err
		}
		if !integrationExists {
			log.Info("integration not found, creating it now", "integration", integrationInput)
			response, err := client.Integrations.CreateIntegration(ctx, &integrationInput)
			if err != nil {
				log.Error(err, fmt.Sprintf("error creating integration %+v", integrationInput))
				return err
			}
			log.Info("created integration", "integration", integrationInput, "response", response)
		}
	}

	return nil
}
