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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	var jujuMachine infrastructurev1beta1.JujuMachine
	if err := r.Get(ctx, req.NamespacedName, &jujuMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get JujuMachine")
		return ctrl.Result{}, err
	}
	log.Info("Retrieved JujuMachine successfully")

	machine, err := util.GetOwnerMachine(ctx, r.Client, jujuMachine.ObjectMeta)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Could not find the owner yet, this is not an error and will rereconcile when the owner gets set.
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get Owner Machine")
		return ctrl.Result{}, err
	}

	if machine == nil {
		log.Info("Machine owner was nil")
		return ctrl.Result{}, nil
	}

	log.Info("Retrieved owner Machine successfully")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JujuMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.JujuMachine{}).
		Complete(r)
}
