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
	"github.com/juju/juju/api/connector"
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
	log.Info("Retrieved JujuMachine successfully", "JujuMachine", jujuMachine)

	machine, err := util.GetOwnerMachine(ctx, r.Client, jujuMachine.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Waiting for machine owner to be found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get Owner Machine")
		return ctrl.Result{}, err
	}

	if machine == nil {
		log.Info("Waiting for machine owner to be non-nil")
		return ctrl.Result{}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, nil
	}

	if !cluster.Status.InfrastructureReady {
		log.Info("Cluster is not ready yet")
		return ctrl.Result{}, nil
	}

	log.Info("Cluster is ready!")

	// Get Infra cluster
	jujuCluster := &infrastructurev1beta1.JujuCluster{}
	objectKey := client.ObjectKey{
		Namespace: jujuMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}

	if err := r.Client.Get(ctx, objectKey, jujuCluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Waiting for JujuCluster to be found")
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
			log.Info("Waiting for juju controller config map to be found")
			return ctrl.Result{Requeue: true}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	// Connect juju API to controller
	// Note this is what Juju calls a controller only connection, only a subnet of the API is available
	// To use the full API you need to specify a model UUID as well
	// https://github.com/juju/juju/blob/d5d762b6053b60ab20b4709c0c3dbfb3deb1d1f4/api/connector.go#L22
	controllerConnector, err := connector.NewSimple(connector.SimpleConfig{
		ControllerAddresses: strings.Split(jujuConfigMap.Data["api_endpoints"], ","),
		CACert:              jujuConfigMap.Data["ca_cert"],
		Username:            jujuConfigMap.Data["user"],
		Password:            jujuConfigMap.Data["password"],
	})
	if err != nil {
		log.Error(err, "failed to create simple connector")
		return ctrl.Result{}, err
	}

	controllerAPI, err := juju.NewJujuAPi(controllerConnector)
	if err != nil {
		log.Error(err, "failed to create juju controller API")
		return ctrl.Result{}, err
	}

	modelUUID, err := controllerAPI.GetModelUUID(jujuCluster.Name)
	if err != nil {
		log.Error(err, "failed to retrieve modelUUID")
		return ctrl.Result{}, err
	}

	// Now that we have the model UUID, we need to make a new api connection including the model UUID
	modelConnector, err := connector.NewSimple(connector.SimpleConfig{
		ControllerAddresses: strings.Split(jujuConfigMap.Data["api_endpoints"], ","),
		CACert:              jujuConfigMap.Data["ca_cert"],
		Username:            jujuConfigMap.Data["user"],
		Password:            jujuConfigMap.Data["password"],
		ModelUUID:           modelUUID,
	})
	if err != nil {
		log.Error(err, "failed to create simple connector")
		return ctrl.Result{}, err
	}

	modelAPI, err := juju.NewJujuAPi(modelConnector)
	if err != nil {
		log.Error(err, "failed to create juju model API")
		return ctrl.Result{}, err
	}

	log.Info("Connected Juju API to controller and model")

	// examine DeletionTimestamp to determine if object is under deletion
	if jujuMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !controllerutil.ContainsFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer) {
			// Refetch the resource before updating
			jujuMachine := &infrastructurev1beta1.JujuMachine{}
			if err := r.Get(ctx, req.NamespacedName, jujuMachine); err != nil {
				if apierrors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				log.Error(err, "failed to get JujuMachine")
				return ctrl.Result{}, err
			}
			log.Info("Refetched jujumachine before performing update for finalizer addition")
			log.Info("Adding finalizer")
			controllerutil.AddFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer)
			if err := r.Update(ctx, jujuMachine); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer) {
			// our finalizer is present, so lets handle controller resource deletion
			log.Info("Finalizer present, handling deletion")
			if jujuMachine.Spec.Machine != nil {
				maxWait := 10 * time.Minute
				machine := jujuMachine.Spec.Machine
				log.Info(fmt.Sprintf("Destroying machine: %s", *machine))
				result, err := modelAPI.DestroyMachine(false, false, false, &maxWait, *machine)
				if err != nil {
					log.Error(err, "Error destroying machine")
					return ctrl.Result{}, err
				}
				if result.Error != nil {
					log.Error(result.Error, "Result contained error")
					return ctrl.Result{}, result.Error
				}
				log.Info("DestroyMachine complete", "result", result)
			}

			// remove our finalizer from the list and update it.
			jujuMachine := &infrastructurev1beta1.JujuMachine{}
			if err := r.Get(ctx, req.NamespacedName, jujuMachine); err != nil {
				if apierrors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				log.Error(err, "failed to get JujuMachine")
				return ctrl.Result{}, err
			}
			log.Info("Refetched jujumachine before updating for finalizer removal")
			controllerutil.RemoveFinalizer(jujuMachine, infrastructurev1beta1.JujuMachineFinalizer)
			if err := r.Update(ctx, jujuMachine); err != nil {
				return ctrl.Result{}, err
			}
		}

		err = controllerAPI.CloseConnections()
		if err != nil {
			return ctrl.Result{}, err
		}

		err = modelAPI.CloseConnections()
		if err != nil {
			return ctrl.Result{}, err
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	jujuMachine = &infrastructurev1beta1.JujuMachine{}
	if err := r.Get(ctx, req.NamespacedName, jujuMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get JujuMachine")
		return ctrl.Result{}, err
	}
	log.Info("Refetched jujumachine before performing nil check")
	if jujuMachine.Spec.Machine == nil {
		log.Info("Machine field in spec was nil, requesting a machine now")
		cons, err := constraints.Parse("cores=2", "mem=8G", "root-disk=16G")
		if err != nil {
			log.Error(err, "error creating machine constraints")
		}
		machineParams := params.AddMachineParams{
			Jobs:        []model.MachineJob{model.JobHostUnits},
			Constraints: cons,
		}
		result, err := modelAPI.AddMachine(machineParams)
		if err != nil {
			log.Error(err, "error adding machine")
			return ctrl.Result{}, err
		}
		if result.Error != nil {
			log.Error(err, "add machine result contains error")
			return ctrl.Result{}, result.Error
		}

		log.Info("machine added", "result", result)
		log.Info(fmt.Sprintf("Updating machine spec to machine: %s", result.Machine))
		// Refetch the resource before updating
		jujuMachine := &infrastructurev1beta1.JujuMachine{}
		if err := r.Get(ctx, req.NamespacedName, jujuMachine); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			log.Error(err, "failed to get JujuMachine")
			return ctrl.Result{}, err
		}
		log.Info("Refetched jujumachine before performing update for machine number")
		machine := result.Machine
		jujuMachine.Spec.Machine = &machine
		if err := r.Update(ctx, jujuMachine); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Successfully updated JujuMachine", "Spec.Machine", jujuMachine.Spec.Machine)
	}

	err = controllerAPI.CloseConnections()
	if err != nil {
		return ctrl.Result{}, err
	}

	err = modelAPI.CloseConnections()
	if err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Stopping reconciliation")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.JujuMachine{}).
		Complete(r)
}
