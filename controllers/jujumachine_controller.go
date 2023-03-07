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
	"fmt"
	"time"

	"github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/instance"
	"github.com/juju/juju/core/model"
	"github.com/juju/juju/rpc/params"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1beta1 "github.com/charmed-kubernetes/cluster-api-provider-juju/api/v1beta1"
)

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
		return ctrl.Result{}, nil
	}

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
	jujuConfig, err := getJujuConfigFromSecret(ctx, jujuCluster, r.Client)
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

	modelUUID, err := client.Models.GetModelUUID(ctx, jujuCluster.Name)
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
			if err := r.Update(ctx, jujuMachine); err != nil {
				return ctrl.Result{}, err
			}
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
					log.Info(fmt.Sprintf("Destroying machine: %s", *machineID))
					result, err := destroyMachine(ctx, *machineID, modelUUID, client)
					if err != nil {
						log.Error(err, fmt.Sprintf("Error destroying machine %s", *jujuMachine.Spec.MachineID))
						return ctrl.Result{}, err
					}
					log.Info("request to destroy machine succeeded, requeueing", "result", result)
					return ctrl.Result{Requeue: true}, err
				}
				// If we make it here, it means the machine was nil and is no longer in the model
				log.Info(fmt.Sprintf("machine %s destroyed", *jujuMachine.Spec.MachineID))
			}

			// remove our finalizer from the list and update it.
			log.Info("removing finalizer")
			controllerutil.RemoveFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer)
			if err := r.Update(ctx, jujuMachine); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		log.Info("stopping reconciliation")
		return ctrl.Result{}, nil
	}

	if jujuMachine.Spec.MachineID == nil {
		log.Info("machineID field in spec was nil, requesting a machine now")
		result, err := createMachine(ctx, modelUUID, client)
		if err != nil {
			return ctrl.Result{}, err
		}

		log.Info("machine added", "result", result)
		log.Info(fmt.Sprintf("updating machine spec to machine: %s", result.Machine))
		machineID := result.Machine
		jujuMachine.Spec.MachineID = &machineID
		if err := r.Update(ctx, jujuMachine); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("successfully updated JujuMachine", "Spec.Machine", jujuMachine.Spec.MachineID)
	}
	if jujuMachine.Spec.ProviderID == nil {
		machineID := jujuMachine.Spec.MachineID
		getMachineInput := juju.GetMachineInput{
			ModelUUID: modelUUID,
			MachineID: *machineID,
		}
		machine, err := client.Machines.GetMachine(ctx, getMachineInput)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to retrieve machine with machine ID %s", *machineID))
			return ctrl.Result{}, err
		}
		log.Info("machine", "status", machine.Status)
		if machine.Status != "started" {
			log.Info("waiting for machine to start, requeueing")
			return ctrl.Result{Requeue: true}, nil
		} else {
			if machine.InstanceId != "" {
				jujuMachine.Spec.ProviderID = &machine.InstanceId
				if err := r.Update(ctx, jujuMachine); err != nil {
					return ctrl.Result{}, err
				}
				log.Info("successfully updated JujuMachine", "Spec.ProviderID", jujuMachine.Spec.ProviderID)
			} else {
				log.Info("machine instance ID was empty, requeueing")
				return ctrl.Result{Requeue: true}, nil
			}
		}

	}

	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("waiting for bootstrap data secret")
		return ctrl.Result{}, nil
	}

	machineID := *jujuMachine.Spec.MachineID
	appsReady, err := r.reconcileApplications(ctx, machine, client, modelUUID, machineID)
	if err != nil {
		log.Error(err, fmt.Sprintf("failed to add units to the machine"))
		return ctrl.Result{}, err
	}

	if !appsReady {
		log.Info("apps not ready yet, requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

	jujuMachine.Status.Ready = true
	err = r.Status().Update(ctx, jujuMachine)
	if err != nil {
		log.Error(err, "failed to update JujuMachine status")
		return ctrl.Result{}, err
	}

	log.Info("stopping reconciliation")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
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
			units, err := client.Applications.AddUnits(ctx, juju.AddUnitsInput{
				ApplicationName: app,
				ModelUUID:       modelUUID,
				NumUnits:        1,
				Placement: []*instance.Placement{
					&instance.Placement{
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

func createMachine(ctx context.Context, modelUUID string, client *juju.Client) (params.AddMachinesResult, error) {
	log := log.FromContext(ctx)
	cons, err := constraints.Parse("cores=2", "mem=8G", "root-disk=16G")
	if err != nil {
		log.Error(err, "error creating machine constraints")
		return params.AddMachinesResult{}, nil
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

func destroyMachine(ctx context.Context, machineID string, modelUUID string, client *juju.Client) (params.DestroyMachineResult, error) {
	input := juju.DestroyMachineInput{
		Force:     true, // FIXME: clean up more cleanly by iterating over units
		Keep:      false,
		DryRun:    false,
		MaxWait:   10 * time.Minute,
		MachineID: machineID,
		ModelUUID: modelUUID,
	}
	return client.Machines.DestroyMachine(ctx, input)
}
