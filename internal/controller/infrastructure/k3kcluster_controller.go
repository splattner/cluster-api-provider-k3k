/*
Copyright 2024.

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

package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1beta1 "github.com/rancher/cluster-api-provider-k3k/api/infrastructure/v1beta1"
)

// K3kClusterReconciler reconciles a K3kCluster object
type K3kClusterReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=k3kclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=k3kclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=k3kclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch

// Reconcile marks the K3kCluster as provisioned. The K3k control plane runs inside the host cluster,
// so there is no infrastructure to provision, and the control plane endpoint is reported by the K3kControlPlane.
func (r *K3kClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	var k3kCluster infrastructurev1beta1.K3kCluster
	if err := r.Get(ctx, req.NamespacedName, &k3kCluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cluster, err := capiutil.GetOwnerCluster(ctx, r.Client, k3kCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get capi cluster owner: %w", err)
	}

	if cluster == nil {
		// the K3kCluster is requeued when the CAPI Cluster controller sets the owner reference
		log.Info("CAPI cluster owner not yet set for K3kCluster")
		return ctrl.Result{}, nil
	}

	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, &k3kCluster); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(&k3kCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		if err := patchHelper.Patch(ctx, &k3kCluster, patch.WithOwnedConditions{Conditions: []string{
			clusterv1beta2.PausedCondition,
			infrastructurev1beta1.K3kClusterReadyCondition,
		}}); err != nil {
			reterr = errors.Join(reterr, err)
		}
	}()

	if !k3kCluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	k3kCluster.Status.Initialization = &infrastructurev1beta1.K3kClusterInitializationStatus{Provisioned: ptr.To(true)}
	k3kCluster.Status.Ready = true

	conditions.Set(&k3kCluster, metav1.Condition{
		Type:   infrastructurev1beta1.K3kClusterReadyCondition,
		Status: metav1.ConditionTrue,
		Reason: infrastructurev1beta1.K3kClusterReadyReason,
	})

	return ctrl.Result{}, nil
}

// InitReconciler sets up the controller with the Manager.
func InitReconciler(ctx context.Context, mgr ctrl.Manager) error {
	log := ctrl.LoggerFrom(ctx)

	log.Info("Init infrastructure Reconciler")

	r := &K3kClusterReconciler{
		Client: mgr.GetClient(),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.K3kCluster{}).
		Watches(
			&clusterv1beta2.Cluster{},
			handler.EnqueueRequestsFromMapFunc(capiutil.ClusterToInfrastructureMapFunc(ctx, infrastructurev1beta1.GroupVersion.WithKind("K3kCluster"), mgr.GetClient(), &infrastructurev1beta1.K3kCluster{})),
			builder.WithPredicates(predicates.ClusterPausedTransitions(mgr.GetScheme(), log)),
		).
		Complete(r)
}
