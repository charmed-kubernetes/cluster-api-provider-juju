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

	"github.com/charmed-kubernetes/cluster-api-provider-juju/juju"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/core/model"
	"github.com/juju/juju/rpc/params"
	kcore "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
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

	// Check if juju config map has been created yet
	jujuConfigMap := &kcore.ConfigMap{}
	objectKey = client.ObjectKey{
		Namespace: jujuCluster.Namespace,
		Name:      jujuCluster.Name + "-juju-controller-config",
	}
	if err := r.Get(ctx, objectKey, jujuConfigMap); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("waiting for juju controller config map to be found")
			return ctrl.Result{Requeue: true}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	connectorConfig := juju.Configuration{
		ControllerAddresses: strings.Split(jujuConfigMap.Data["api_endpoints"], ","),
		Username:            jujuConfigMap.Data["user"],
		Password:            jujuConfigMap.Data["password"],
		CACert:              jujuConfigMap.Data["ca_cert"],
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
				log.Info(fmt.Sprintf("Destroying machine: %s", *machineID))
				result, err := destroyMachine(ctx, *machineID, modelUUID, client)
				if err != nil {
					return ctrl.Result{}, err
				}
				log.Info("machine destroyed", "result", result)
			}

			// remove our finalizer from the list and update it.
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

	// TODO: Set provider ID when machine is running/idle

	log.Info("stopping reconciliation")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.JujuMachine{}).
		Complete(r)
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
		Force:     false,
		Keep:      false,
		DryRun:    false,
		MaxWait:   10 * time.Minute,
		MachineID: machineID,
		ModelUUID: modelUUID,
	}
	return client.Machines.DestroyMachine(ctx, input)
}
