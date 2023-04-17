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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	infrastructurev1beta1 "github.com/charmed-kubernetes/cluster-api-provider-juju/api/v1beta1"
	"github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/instance"
	"github.com/juju/juju/core/model"
	"github.com/juju/juju/rpc/params"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/connrotation"
	"k8s.io/client-go/util/workqueue"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type kubernetesClient struct {
	*kubernetes.Clientset
	dialer *connrotation.Dialer
}

// Close kubernetes client.
func (k *kubernetesClient) Close() error {
	k.dialer.CloseAll()
	return nil
}

func newDialer() *connrotation.Dialer {
	return connrotation.NewDialer((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
}

// JujuMachineReconciler reconciles a JujuMachine object
type JujuMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujumachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujumachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=jujumachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the JujuMachine object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *JujuMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	jujuMachine := &infrastructurev1beta1.JujuMachine{}
	if err := r.Get(ctx, req.NamespacedName, jujuMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get JujuMachine")
		return ctrl.Result{}, err
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, jujuMachine.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("waiting for machine owner to be found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get Owner Machine")
		return ctrl.Result{}, err
	}

	if machine == nil {
		log.Info("waiting for machine owner to be non-nil")
		return ctrl.Result{}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, nil
	}

	if !cluster.Status.InfrastructureReady {
		log.Info("cluster is not ready yet")
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(jujuMachine, r.Client)
	if err != nil {
		log.Error(err, "failed to configure the patch helper")
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the JujuMachine object and status after each reconciliation.
	defer func() {
		log.Info("patching jujuMachine")
		if e := patchHelper.Patch(ctx, jujuMachine); e != nil {
			log.Error(e, "failed to patch jujuMachine")

			if err == nil {
				err = e
			}
		}
	}()

	// Get Infra cluster
	jujuCluster := &infrastructurev1beta1.JujuCluster{}
	objectKey := client.ObjectKey{
		Namespace: jujuMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err := r.Client.Get(ctx, objectKey, jujuCluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("waiting for JujuCluster to be found")
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "failed to get JujuCluster")
		return ctrl.Result{}, err
	}

	// Get config data from secret
	jujuConfig, err := getJujuConfigFromSecret(ctx, cluster, jujuCluster, r.Client)
	if err != nil {
		log.Error(err, "failed to retrieve juju configuration data from secret")
		return ctrl.Result{}, err
	}
	if jujuConfig == nil {
		log.Info("juju controller configuration was nil, requeuing")
		return ctrl.Result{RequeueAfter: requeueTime}, nil
	}

	connectorConfig := juju.Configuration{
		ControllerAddresses: jujuConfig.Details.APIEndpoints,
		Username:            jujuConfig.Account.User,
		Password:            jujuConfig.Account.Password,
		CACert:              jujuConfig.Details.CACert,
	}
	client, err := juju.NewClient(connectorConfig)
	if err != nil {
		log.Error(err, "failed to create juju client")
		return ctrl.Result{}, err
	}

	modelUUID, err := client.Models.GetModelUUID(ctx, jujuCluster.Spec.Model.Name)
	if err != nil {
		log.Error(err, "failed to retrieve modelUUID")
		return ctrl.Result{}, err
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if jujuMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer) {
			controllerutil.AddFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer)
			log.Info("added finalizer")
			return ctrl.Result{}, nil
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer) {
			// our finalizer is present, so lets handle controller resource deletion
			if jujuMachine.Spec.MachineID != nil {
				machineID := jujuMachine.Spec.MachineID
				getMachineInput := juju.GetMachineInput{
					ModelUUID: modelUUID,
					MachineID: *machineID,
				}
				machine, err := client.Machines.GetMachine(ctx, getMachineInput)
				if err != nil {
					log.Error(err, "error getting machine when attempting deletion")
					return ctrl.Result{}, err
				}
				if machine != nil {
					log.Info("machine information", "machine", machine)
					log.Info(fmt.Sprintf("Destroying machine %s by removing all current units", *machineID))
					err := destroyMachine(ctx, *machineID, modelUUID, client)
					if err != nil {
						log.Error(err, fmt.Sprintf("Error destroying machine %s", *jujuMachine.Spec.MachineID))
						return ctrl.Result{}, err
					}
					return ctrl.Result{Requeue: true}, err
				}
				// If we make it here, it means the machine was nil and is no longer in the model
				log.Info(fmt.Sprintf("machine %s destroyed", *jujuMachine.Spec.MachineID))
			}

			// remove our finalizer
			log.Info("removing finalizer")
			controllerutil.RemoveFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer)
		}

		// Stop reconciliation as the item is being deleted
		log.Info("stopping reconciliation")
		return ctrl.Result{}, nil
	}

	if jujuMachine.Spec.MachineID == nil {
		log.Info("machineID field in spec was nil, requesting a machine now")
		result, err := createMachine(ctx, modelUUID, jujuMachine, client)
		if err != nil {
			return ctrl.Result{}, err
		}

		log.Info("machine added", "result", result)
		log.Info(fmt.Sprintf("updating machine spec to machine: %s", result.Machine))
		machineID := result.Machine
		jujuMachine.Spec.MachineID = &machineID
		return ctrl.Result{Requeue: true}, nil
	}

	getMachineInput := juju.GetMachineInput{
		ModelUUID: modelUUID,
		MachineID: *jujuMachine.Spec.MachineID,
	}
	m, err := client.Machines.GetMachine(ctx, getMachineInput)
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to retrieve machine with machine ID %s", *jujuMachine.Spec.MachineID))
		return ctrl.Result{}, err
	}
	if m.Status != "started" {
		log.Info("waiting for machine to start, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("waiting for bootstrap data secret")
		return ctrl.Result{}, nil
	}

	appsReady, err := r.reconcileApplications(ctx, machine, client, modelUUID, *jujuMachine.Spec.MachineID)
	if err != nil {
		log.Error(err, "failed to add units to the machine")
		return ctrl.Result{}, err
	}

	if !appsReady {
		log.Info("apps not ready yet, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	if jujuMachine.Spec.ProviderID == nil {
		if m.InstanceId != "" {
			kubeclient, err := r.kubeClientForCluster(ctx, cluster)
			if err != nil {
				if apierrors.IsNotFound(err) {
					log.Info("kubeconfig secret not found yet, requeueing")
					return ctrl.Result{Requeue: true}, nil
				} else {
					log.Error(err, "failed to create kubernetes client")
					return ctrl.Result{}, err
				}
			}

			defer kubeclient.Close()

			ipInput := juju.GetMachineAddressesInput{
				ModelUUID: modelUUID,
				MachineID: *jujuMachine.Spec.MachineID,
			}
			ipAddresses, err := client.Machines.GetMachineIPs(ctx, ipInput)
			if err != nil {
				log.Error(err, "error getting machine IPs")
				return ctrl.Result{}, err
			}

			if len(ipAddresses) < 1 {
				return ctrl.Result{}, errors.New("machine IP addresses were empty")
			}

			node, err := getNodeWithAddresses(ctx, ipAddresses, kubeclient)
			if err != nil {
				log.Error(err, "error getting node")
				return ctrl.Result{}, err
			}

			if node == nil {
				log.Info("did not match node with addresses, requeueing", "addresses", ipAddresses)
				return ctrl.Result{Requeue: true}, nil
			}

			// 2 possible cases
			// Either we arent expecting a cloud provider to set the provider ID, so we will create our own juju provider ID
			// based on juju instance ID
			// or we are expecting a cloud provider to set the provider ID, and have to retrieve it
			// Either way the node provider ID and machine provider ID will need to match
			if jujuMachine.Spec.UseJujuProviderID {
				providerID := fmt.Sprintf("juju://%s", m.InstanceId)
				jujuMachine.Spec.ProviderID = &providerID
				log.Info("updated JujuMachine providerID", "Spec.ProviderID", jujuMachine.Spec.ProviderID)
				// update the nodes provider ID to match
				node.Spec.ProviderID = *jujuMachine.Spec.ProviderID
				updatedNode, err := kubeclient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
				if err != nil {
					log.Error(err, "failed to update node")
					return ctrl.Result{}, err
				}
				log.Info("updated node provider ID", "node", updatedNode.Name, "providerId", updatedNode.Spec.ProviderID)
			} else {
				log.Info("UseJujuProviderID is false, will get providerID from corresponding node")

				if node.Spec.ProviderID == "" {
					log.Info("provider ID was empty, requeuing until cloud provider sets it")
					return ctrl.Result{Requeue: true}, nil
				} else {
					jujuMachine.Spec.ProviderID = &node.Spec.ProviderID
					log.Info("updated JujuMachine providerID", "Spec.ProviderID", jujuMachine.Spec.ProviderID)
				}
			}
		} else {
			log.Info("machine instance ID was empty, requeueing")
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// machine is ready at this point
	jujuMachine.Status.Ready = true

	// update IPs
	machineAddresses, err := buildMachineAddresses(ctx, modelUUID, *jujuMachine.Spec.MachineID, client)
	if err != nil {
		log.Error(err, "error building machine addresses")
		return ctrl.Result{}, err
	}

	log.Info("Updating machine addresses", "addresses", machineAddresses)
	jujuMachine.Status.Addresses = machineAddresses

	log.Info("stopping reconciliation")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewMaxOfRateLimiter(
				workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 5*time.Minute),
				&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
			),
		}).
		For(&infrastructurev1beta1.JujuMachine{}).
		Complete(r)
}

func (r *JujuMachineReconciler) reconcileApplications(ctx context.Context, machine *clusterv1.Machine, client *juju.Client, modelUUID string, machineID string) (bool, error) {
	log := log.FromContext(ctx)

	namespace := machine.Namespace
	dataSecretName := *machine.Spec.Bootstrap.DataSecretName
	bootstrapData, err := r.getBootstrapData(ctx, namespace, dataSecretName)
	if err != nil {
		log.Error(err, "failed to get bootstrap data")
		return false, err
	}

	appsReady := true

	for _, app := range bootstrapData {
		unitStatus, err := getUnitStatusForAppOnMachine(ctx, client, modelUUID, app, machineID)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to get unit status for app %s on machine %s", app, machineID))
			return false, err
		}

		if unitStatus == nil {
			appsReady = false
			units, err := client.Applications.AddUnits(ctx, juju.AddUnitsInput{
				ApplicationName: app,
				ModelUUID:       modelUUID,
				NumUnits:        1,
				Placement: []*instance.Placement{
					{
						Scope:     instance.MachineScope,
						Directive: machineID,
					},
				},
			})
			if err != nil {
				log.Error(err, fmt.Sprintf("failed to add unit for app %s to machine %s in model %s", app, machineID, modelUUID))
				return false, err
			}
			log.Info(fmt.Sprintf("successfully added unit %s to machine %s in model %s", units, machineID, modelUUID))
		} else if unitStatus.WorkloadStatus.Status == "active" && unitStatus.AgentStatus.Status == "idle" {
			log.Info(fmt.Sprintf("application %s is ready on machine %s", app, machineID))
		} else {
			appsReady = false
			log.Info(fmt.Sprintf("application %s is not ready on machine %s, status: %+v", app, machineID, unitStatus))
		}
	}

	return appsReady, nil
}

func (r *JujuMachineReconciler) getBootstrapData(ctx context.Context, namespace string, dataSecretName string) ([]string, error) {
	log := log.FromContext(ctx)

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dataSecretName}, secret)
	if err != nil {
		log.Error(err, "failed to get bootstrap data secret")
		return nil, err
	}

	if !bytes.Equal(secret.Data["format"], []byte("juju")) {
		return nil, fmt.Errorf("bootstrap data secret has the wrong format")
	}

	var bootstrapData []string
	err = json.Unmarshal(secret.Data["value"], &bootstrapData)
	if err != nil {
		log.Error(err, "failed to unmarshal bootstrap data value")
		return nil, err
	}

	return bootstrapData, nil
}

func getUnitStatusForAppOnMachine(ctx context.Context, client *juju.Client, modelUUID string, applicationName string, machineID string) (*params.UnitStatus, error) {
	log := log.FromContext(ctx)

	readAppInput := juju.ReadApplicationInput{
		ModelUUID:       modelUUID,
		ApplicationName: applicationName,
	}
	readAppResponse, err := client.Applications.ReadApplication(ctx, &readAppInput)
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to read application %s in model %s", applicationName, modelUUID))
		return nil, err
	}

	for _, unitStatus := range readAppResponse.Status.Units {
		if unitStatus.Machine == machineID {
			return &unitStatus, nil
		}
	}

	return nil, nil
}

func createMachine(ctx context.Context, modelUUID string, machine *infrastructurev1beta1.JujuMachine, client *juju.Client) (params.AddMachinesResult, error) {
	cons := constraints.Value{}
	if machine.Spec.Constraints != nil {
		cons = constraints.Value(*machine.Spec.Constraints)
	}

	machineParams := params.AddMachineParams{
		Jobs:        []model.MachineJob{model.JobHostUnits},
		Constraints: cons,
	}
	input := juju.AddMachineInput{
		MachineParams: machineParams,
		ModelUUID:     modelUUID,
	}

	return client.Machines.AddMachine(ctx, input)
}

func destroyMachine(ctx context.Context, machineID string, modelUUID string, client *juju.Client) error {
	log := log.FromContext(ctx)
	// Loop over units on the machine and remove them
	// Removing all units should force the machine to be removed as well

	// Getting units for a specific machine is a bit tricky, must get applications status, and then find the units belonging to the
	// machine we are looking for within that
	appStatuses, err := client.Applications.GetApplicationsStatus(ctx, modelUUID)
	if err != nil {
		return err
	}

	units := make(map[string]params.UnitStatus)
	for _, appStatus := range appStatuses {
		for unitID, unitStatus := range appStatus.Units {
			if unitStatus.Machine == machineID {
				// we only want to remove those that are not currently dying
				// if they are dying theyve already had a destroy call made
				// Juju does not seem to update workload status.Life when removing units, it does update agent status though
				if unitStatus.AgentStatus.Life != "dying" {
					log.Info(fmt.Sprintf("%s is not yet dying, will be removing this unit", unitID))
					units[unitID] = unitStatus
				}
			}
		}
	}

	for unitID, unitStatus := range units {
		// remove each unit, taking care to force the unit removal if the workload status is error
		// note that we do send a separate request for each removal even though the applicationAPI can handle removing multiple units with one call
		// but its all or nothing with the force parameter. We only want to force remove when necessary
		log.Info("removing unit from machine", "unit", unitID, "machine", machineID)
		inError := unitStatus.WorkloadStatus.Status == "error" || unitStatus.AgentStatus.Status == "error"
		if inError {
			log.Info("unit is in error state, removing with force")
		}
		destroyUnitInput := juju.DestroyUnitsInput{
			Units:     []string{unitID},
			ModelUUID: modelUUID,
			Force:     inError,
		}

		err := client.Applications.DestroyUnits(ctx, destroyUnitInput)
		if err != nil {
			return err
		}
	}

	if len(units) == 0 {
		log.Info(fmt.Sprintf("all units on machine %s are currently in the process of dying. waiting for that to complete", machineID))
	}
	return nil
}

// kubeClientForCluster will fetch a kubeconfig secret based on cluster name/namespace,
// use it to create a clientset, and return a kubernetes client.
func (r *JujuMachineReconciler) kubeClientForCluster(ctx context.Context, cluster *clusterv1.Cluster) (*kubernetesClient, error) {
	log := log.FromContext(ctx)

	kubeConfigSecret := &corev1.Secret{}
	objectKey := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Name + "-kubeconfig",
	}

	err := r.Get(ctx, objectKey, kubeConfigSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("kubeconfig secret not found")
			return nil, err
		} else {
			log.Error(err, "error getting kubeconfig secret")
			return nil, err
		}
	}

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigSecret.Data["value"])
	if err != nil {
		log.Error(err, "error creating rest config")
		return nil, err
	}

	dialer := newDialer()
	config.Dial = dialer.DialContext

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &kubernetesClient{
		Clientset: clientset,
		dialer:    dialer,
	}, nil
}

func getNodeWithAddresses(ctx context.Context, addresses []string, kubeclient *kubernetesClient) (*corev1.Node, error) {
	log := log.FromContext(ctx)
	nodes, err := kubeclient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Error(err, "error listing nodes")
		return nil, err
	}

	for _, node := range nodes.Items {
		for _, nodeAddress := range node.Status.Addresses {
			for _, machineAddress := range addresses {
				if nodeAddress.Address == machineAddress {
					return &node, nil
				}
			}
		}
	}
	return nil, nil
}

func buildMachineAddresses(ctx context.Context, modelUUID string, machineID string, client *juju.Client) ([]clusterv1.MachineAddress, error) {
	ipInput := juju.GetMachineAddressesInput{
		ModelUUID: modelUUID,
		MachineID: machineID,
	}
	ipAddresses, err := client.Machines.GetMachineIPs(ctx, ipInput)
	if err != nil {
		return nil, err
	}
	if len(ipAddresses) > 0 {
		machineAddresses := []clusterv1.MachineAddress{}
		for _, ipAddress := range ipAddresses {
			machineAddress := clusterv1.MachineAddress{
				Type:    clusterv1.MachineInternalIP,
				Address: ipAddress,
			}
			machineAddresses = append(machineAddresses, machineAddress)
		}
		return machineAddresses, nil
	} else {
		return nil, errors.New("machine IP addresses were empty")
	}
}
