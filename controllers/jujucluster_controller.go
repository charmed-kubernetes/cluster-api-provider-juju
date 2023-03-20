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
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const requeueTime = 10 * time.Second
const controllerDataSecretName = "juju-controller-data"
const jujuAccountName = "capi-juju"

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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	// Ensure the job has completed
	if bootstrapJob.Status.Succeeded <= 0 {
		log.Info("waiting for bootstrap job to complete")
		return ctrl.Result{Requeue: true}, nil
	}

	// Get config data from secret
	jujuConfig, err := getJujuConfigFromSecret(ctx, jujuCluster, r.Client)
	if err != nil {
		log.Error(err, "failed to retrieve juju configuration data from secret")
		return ctrl.Result{}, err
	}
	if jujuConfig == nil {
		log.Error(err, "juju controller configuration was nil")
		return ctrl.Result{}, err
	}

	connectorConfig := juju.Configuration{
		ControllerAddresses: jujuConfig.Details.APIEndpoints,
		Username:            jujuConfig.Account.User,
		Password:            jujuConfig.Account.Password,
		CACert:              jujuConfig.Details.CACert,
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
								// Neither the applications API nor the model API provide a way of getting at existing apps directly,
								// so we check status here to get application names
								appStatuses, err := jujuClient.Applications.GetApplicationsStatus(ctx, modelUUID)
								if err != nil {
									log.Error(err, "failed to get application statuses")
									return ctrl.Result{}, err
								}
								destroyedApps := []string{}
								for appName := range appStatuses {
									appExistsInput := juju.ApplicationExistsInput{
										ModelUUID:       modelUUID,
										ApplicationName: appName,
									}
									appExists, err := jujuClient.Applications.ApplicationExists(ctx, appExistsInput)
									if err != nil {
										log.Error(err, fmt.Sprintf("failed to query existing applications while checking if application %s exists", appName))
										return ctrl.Result{}, err
									}
									if appExists {
										log.Info(fmt.Sprintf("destroying %s", appName))
										destroyAppInput := juju.DestroyApplicationInput{
											ApplicationName: appName,
											ModelUUID:       modelUUID,
										}
										if err := jujuClient.Applications.DestroyApplication(ctx, &destroyAppInput); err != nil {
											log.Error(err, fmt.Sprintf("failed to destroy %s", appName))
											return ctrl.Result{}, err
										}
										destroyedApps = append(destroyedApps, appName)
									}
								}
								if len(destroyedApps) > 0 {
									log.Info("requeueing after destroying applications", "applications", destroyedApps)
									return ctrl.Result{RequeueAfter: requeueTime}, nil
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
								return ctrl.Result{RequeueAfter: requeueTime}, nil
							} else {
								log.Error(nil, "model was alive and in a non-destroying state, but the model UUID returned was nil", "model", jujuCluster.Name)
								return ctrl.Result{RequeueAfter: requeueTime}, nil
							}
						} else {
							log.Info("model is being destroyed, requeueing")
							return ctrl.Result{RequeueAfter: requeueTime}, nil
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

	err = r.createRegistrationSecretIfNeeded(ctx, jujuCluster, *jujuConfig, jujuClient)
	if err != nil {
		log.Error(err, "error creating registration secret")
		return ctrl.Result{}, err
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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	modelUUID, err := jujuClient.Models.GetModelUUID(ctx, jujuCluster.Name)
	if err != nil {
		log.Error(err, "failed to retrieve modelUUID")
		return ctrl.Result{}, err
	}

	createdApplications, err := createApplicationsIfNeeded(ctx, jujuCluster, jujuClient, modelUUID)
	if err != nil {
		log.Error(err, "error creating applications")
		return ctrl.Result{}, err
	}
	if createdApplications {
		log.Info("applications were created, requeuing")
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	createdIntegrations, err := createIntegrationsIfNeeded(ctx, jujuCluster, jujuClient, modelUUID)
	if err != nil {
		log.Error(err, "error creating integrations")
		return ctrl.Result{}, err
	}
	if createdIntegrations {
		log.Info("integrations were created, requeuing")
		return ctrl.Result{RequeueAfter: requeueTime}, nil
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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
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
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	if jujuCluster.Spec.ControlPlaneEndpoint.Host == "" {
		log.Info("cluster control plane endpoint host was empty, setting it now")
		address, err := getLoadBalancerAddress(ctx, jujuCluster, jujuClient, modelUUID)
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
		Owns(&kcore.Secret{}).
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
		"./juju add-user %s > NUL;"+
		"./juju grant capi-juju superuser;"+
		"./juju show-controller --show-password > controller_data.txt;"+
		"./kubectl create secret generic %s --from-file=controller-data=./controller_data.txt -n %s", cloudName, cloudName, jujuCluster.Spec.ControllerServiceType, jujuAccountName, controllerDataSecretName, jujuCluster.Namespace)
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

	configSecret := &kcore.Secret{}
	configSecretKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      controllerDataSecretName,
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
		{configSecret, configSecretKey, client.DeleteOptions{}},
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

func getJujuConfigFromSecret(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, c client.Client) (*JujuConfig, error) {
	log := log.FromContext(ctx)

	configSecret := &kcore.Secret{}
	objectKey := client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      controllerDataSecretName,
	}
	if err := c.Get(ctx, objectKey, configSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, err
		}
	}
	data := string(configSecret.Data["controller-data"][:])
	split := strings.Split(data, fmt.Sprintf("%s:\n", jujuCluster.Name+"-k8s-cloud"))
	yam := split[1]
	config := JujuConfig{}
	err := yaml.Unmarshal([]byte(yam), &config)
	if err != nil {
		log.Error(err, "error unmarshalling YAML data into config struct")
		return nil, err
	}

	return &config, nil

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

func getLoadBalancerAddress(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (*string, error) {
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

func createIntegrationsIfNeeded(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (bool, error) {
	log := log.FromContext(ctx)
	created := false
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
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"containerd:containerd", "kubernetes-worker:container-runtime"},
		},
		{
			ModelUUID: modelUUID,
			Endpoints: []string{"containerd:containerd", "kubernetes-control-plane:container-runtime"},
		},
	}

	for _, integrationInput := range integrationInputs {
		integrationExists, err := client.Integrations.IntegrationExists(ctx, &integrationInput)
		if err != nil {
			log.Error(err, fmt.Sprintf("Error querying existing integrations for integration: %+v", integrationInput))
			return created, err
		}
		if !integrationExists {
			log.Info("integration not found, creating it now", "integration", integrationInput)
			response, err := client.Integrations.CreateIntegration(ctx, &integrationInput)
			if err != nil {
				log.Error(err, fmt.Sprintf("error creating integration %+v", integrationInput))
				return created, err
			}
			log.Info("created integration", "integration", integrationInput, "response", response)
			created = true
		}
	}

	return created, nil
}

func createApplicationsIfNeeded(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, client *juju.Client, modelUUID string) (bool, error) {
	log := log.FromContext(ctx)
	created := false
	easyRSACons, err := constraints.Parse("cores=1", "mem=4G", "root-disk=16G")
	if err != nil {
		log.Error(err, "error creating machine constraints")
		return false, err
	}

	lbCons, err := constraints.Parse("cores=1", "mem=4G", "root-disk=16G")
	if err != nil {
		log.Error(err, "error creating machine constraints")
		return created, err
	}

	createInputs := []juju.CreateApplicationInput{
		{
			ApplicationName: "easyrsa",
			ModelUUID:       modelUUID,
			CharmName:       "easyrsa",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           1,
			Constraints:     easyRSACons,
		},
		{
			ApplicationName: "kubeapi-load-balancer",
			ModelUUID:       modelUUID,
			CharmName:       "kubeapi-load-balancer",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           1,
			Constraints:     lbCons,
		},
		{
			ApplicationName: "kubernetes-control-plane",
			ModelUUID:       modelUUID,
			CharmName:       "kubernetes-control-plane",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           0,
			Config: map[string]interface{}{
				"ignore-missing-cni":      true,
				"enable-metrics":          false,
				"dns-provider":            "none",
				"enable-dashboard-addons": false,
			},
		},
		{
			ApplicationName: "kubernetes-worker",
			ModelUUID:       modelUUID,
			CharmName:       "kubernetes-worker",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           0,
			Config: map[string]interface{}{
				"ignore-missing-cni": true,
				"ingress":            false,
			},
		},
		{
			ApplicationName: "etcd",
			ModelUUID:       modelUUID,
			CharmName:       "etcd",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           0,
		},
		{
			ApplicationName: "containerd",
			ModelUUID:       modelUUID,
			CharmName:       "containerd",
			CharmChannel:    "1.27/edge",
			CharmBase:       "ubuntu@22.04",
			Units:           0,
		},
	}

	for _, createInput := range createInputs {
		existsInput := juju.ApplicationExistsInput{
			ModelUUID:       modelUUID,
			ApplicationName: createInput.ApplicationName,
		}
		exists, err := client.Applications.ApplicationExists(ctx, existsInput)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to query existing applications while checking if %s exists", existsInput.ApplicationName))
			return created, err
		}
		if !exists {
			log.Info(fmt.Sprintf("application %s not found, creating it now", createInput.ApplicationName))
			response, err := client.Applications.CreateApplication(ctx, &createInput)
			if err != nil {
				log.Error(err, fmt.Sprintf("error creating application %s", createInput.ApplicationName))
				return created, err
			}

			log.Info("created application", "response", response)
			created = true
		}

	}

	return created, nil
}

func (r *JujuClusterReconciler) createRegistrationSecretIfNeeded(ctx context.Context, jujuCluster *infrastructurev1beta1.JujuCluster, config JujuConfig, client *juju.Client) error {
	log := log.FromContext(ctx)

	registrationSecret := &kcore.Secret{}
	registrationSecret.Name = jujuCluster.Name + "-registration-string"
	registrationSecret.Namespace = jujuCluster.Namespace

	err := r.Get(ctx, types.NamespacedName{Name: registrationSecret.Name, Namespace: registrationSecret.Namespace}, registrationSecret)
	if err != nil && apierrors.IsNotFound(err) {
		log.Info("creating registration secret")
		registrationString, err := client.Users.ResetPassword(ctx, jujuCluster.Name+"-k8s-cloud", config.Details.APIEndpoints, jujuAccountName)
		if err != nil {
			log.Error(err, fmt.Sprintf("error resetting password for %s", jujuAccountName))
			return err
		}
		registrationSecret.Type = kcore.SecretTypeOpaque
		registrationSecret.Data = map[string][]byte{}
		registrationSecret.Data["registration-string"] = []byte(registrationString)

		if err := controllerutil.SetControllerReference(jujuCluster, registrationSecret, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, registrationSecret); err != nil {
			log.Error(err, "failed to create registration secret")
			return err
		}

	}

	return nil
}
