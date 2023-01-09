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

	infrastructurev1beta1 "github.com/charmed-kubernetes/cluster-api-provider-juju/api/v1beta1"
	"github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/juju/juju/api/connector"
	"github.com/juju/juju/cloud"
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
			log.Info("Waiting for cluster owner to be found")
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "failed to get owner Cluster")
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("Waiting for cluster owner to be non-nil")
		return ctrl.Result{Requeue: true}, nil
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

	// Check if juju config map has been created yet
	// Check if a juju controller has been created yet via the job
	jujuConfigMap := &kcore.ConfigMap{}
	objectKey = client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-config",
	}
	if err := r.Get(ctx, objectKey, jujuConfigMap); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("juju controller config not found, creating it now")
			if err := r.createJujuConfigMap(ctx, cluster, jujuCluster); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	// Connect juju API to controller
	// https://github.com/juju/juju/blob/d5d762b6053b60ab20b4709c0c3dbfb3deb1d1f4/api/connector.go#L22
	connector, err := connector.NewSimple(connector.SimpleConfig{
		ControllerAddresses: strings.Split(jujuConfigMap.Data["api_endpoints"], ","),
		CACert:              jujuConfigMap.Data["ca_cert"],
		Username:            jujuConfigMap.Data["user"],
		Password:            jujuConfigMap.Data["password"],
	})

	if err != nil {
		log.Error(err, "failed to create simple connector")
		return ctrl.Result{}, err
	}

	jujuAPI, err := juju.NewJujuAPi(connector)
	if err != nil {
		log.Error(err, "failed to create juju API")
		return ctrl.Result{}, err
	}

	log.Info("Connected Juju API to controller")
	cloudExists, err := jujuAPI.CloudExists("capi-vsphere")
	if err != nil {
		log.Error(err, "failed to query existing clouds")
		return ctrl.Result{}, err
	}

	if !cloudExists {
		log.Info("cloud not found, creating it now")
		vsphereRegion := cloud.Region{
			Name:     "Boston",
			Endpoint: "10.246.152.100",
		}

		vsphereCloud := cloud.Cloud{
			Name:      "capi-vsphere",
			Type:      "vsphere",
			Endpoint:  "10.246.152.100",
			Regions:   []cloud.Region{vsphereRegion},
			AuthTypes: []cloud.AuthType{"userpass"},
		}

		err = jujuAPI.AddCloud(vsphereCloud, true)
		if err != nil {
			log.Error(err, "failed to add cloud")
			return ctrl.Result{}, err
		}
	}

	credentialExists, err := jujuAPI.CredentialExists("capi-vsphere", "capi-vsphere")
	if err != nil {
		log.Error(err, "failed to query existing credentials")
		return ctrl.Result{}, err
	}
	if !credentialExists {
		log.Info("credential not found, creating it now")
		credential := cloud.NewCredential("userpass", map[string]string{
			"username": "k8s-crew@vsphere.local",
			"password": "secret",
			"vmfolder": "vmfolder",
		})
		err = jujuAPI.AddCredential(credential, "capi-vsphere", "capi-vsphere")
		if err != nil {
			log.Error(err, "failed to add credential")
			return ctrl.Result{}, err
		}
	}

	log.Info("Stopping reconciliation")
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
		"./juju bootstrap %s;"+
		"./juju add-user capi-juju;"+
		"./juju grant capi-juju superuser;"+
		"./juju show-controller --show-password", cloudName, cloudName)
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
