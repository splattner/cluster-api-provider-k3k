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

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/rancher/k3k/pkg/controller/kubeconfig"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upstream "github.com/rancher/k3k/pkg/apis/k3k.io/v1beta1"
	k3kcontroller "github.com/rancher/k3k/pkg/controller"
	v1 "k8s.io/api/core/v1"
	apiError "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiutil "sigs.k8s.io/cluster-api/util"
	capiKubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	ctrl "sigs.k8s.io/controller-runtime"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"
	"github.com/rancher/cluster-api-provider-k3k/internal/helm"
)

const (
	ownerNameLabel      = "app.cattle.io/owner-name"
	ownerNamespaceLabel = "app.cattle.io/owner-ns"
	finalizer           = "app.cattle.io/upstream-remove"
)

type scope struct {
	logr.Logger

	k3kControlPlane *controlplanev1.K3kControlPlane
	cluster         *clusterv1beta2.Cluster
	// hostConfig is the config of the host cluster, nil for the management cluster
	hostConfig *rest.Config
}

// K3kControlPlaneReconciler reconciles a K3kControlPlane object
type K3kControlPlaneReconciler struct {
	client.Client
	Host client.Client
	Helm *helm.Client
}

// InitReconciler sets up the controller with the Manager.
func InitReconciler(ctx context.Context, mgr ctrl.Manager) error {
	log := ctrl.LoggerFrom(ctx)

	log.Info("Init controlplane Reconciler")

	restClientGetter, err := helm.NewRESTClientGetter(mgr.GetConfig(), mgr.GetRESTMapper())
	if err != nil {
		log.Error(err, "failed to set up REST client getter")
		return err
	}

	helmClient, err := helm.New(ctx, restClientGetter)
	if err != nil {
		log.Error(err, "failed to set up Helm client")
		return err
	}

	// Add k3k repository
	if err := helmClient.AddRepository(ctx, helm.K3kRepoName, helm.K3kRepoURL); err != nil {
		return fmt.Errorf("failed to add k3k repository: %w", err)
	}

	r := &K3kControlPlaneReconciler{
		Client: mgr.GetClient(),
		Helm:   helmClient,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1.K3kControlPlane{}).
		Watches(
			&clusterv1beta2.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToK3kControlPlane),
			builder.WithPredicates(predicates.ClusterPausedTransitions(mgr.GetScheme(), log)),
		).
		Complete(r)
}

// clusterToK3kControlPlane maps a CAPI Cluster to the K3kControlPlane it references.
func clusterToK3kControlPlane(_ context.Context, o client.Object) []reconcile.Request {
	cluster, ok := o.(*clusterv1beta2.Cluster)
	if !ok {
		return nil
	}

	ref := cluster.Spec.ControlPlaneRef
	if ref.Kind != "K3kControlPlane" || ref.APIGroup != controlplanev1.GroupVersion.Group {
		return nil
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}}}
}

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=k3kcontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=k3kcontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=k3kcontrolplanes/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=k3k.io,resources=clusters,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=clusterroles,verbs=bind;get;create
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=clusterrolebindings,verbs=get;create
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=namespaces;serviceaccounts,verbs=get;list;watch;create
//+kubebuilder:rbac:groups="",resources=nodes;nodes/proxy,verbs=get;list;watch
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;create
//+kubebuilder:rbac:groups="apps",resources=replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups="apiextensions.k8s.io",resources=customresourcedefinitions,verbs=get;list;create
//+kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=*

// Reconcile creates a K3k Upstream cluster based on the provided spec of the K3kControlPlane.
func (r *K3kControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	var k3kControlPlane controlplanev1.K3kControlPlane
	if err := r.Get(ctx, req.NamespacedName, &k3kControlPlane); err != nil {
		if apiError.IsNotFound(err) {
			log.Error(err, "couldn't find controlplane")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("unable to get controlplane for request %w", err)
	}

	r.Host = r.Client

	// the host cluster config, nil for the management cluster
	var hostConfig *rest.Config

	if k3kControlPlane.Spec.HostKubeconfig != nil {
		log.Info("targeting external host cluster")

		hostKubeconfigSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      k3kControlPlane.Spec.HostKubeconfig.SecretName,
				Namespace: k3kControlPlane.Spec.HostKubeconfig.SecretNamespace,
			},
		}

		if err := r.Get(ctx, client.ObjectKeyFromObject(hostKubeconfigSecret), hostKubeconfigSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get host kubeconfig secret: %w", err)
		}

		config, err := clientcmd.RESTConfigFromKubeConfig(hostKubeconfigSecret.Data["value"])
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create RESTConfig from host kubeconfig secret: %w", err)
		}

		hostConfig = config

		hostClient, err := client.New(config, client.Options{Scheme: r.Scheme()})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create host client: %w", err)
		}

		r.Host = hostClient
	}

	cluster, err := capiutil.GetOwnerCluster(ctx, r, k3kControlPlane.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get capi cluster owner: %w", err)
	}

	if cluster == nil {
		// capi cluster owner may not be set immediately, but we don't want to process the cluster until it is
		log.Info("CAPI cluster owner not yet set for K3kControlPlane", "controlPlane", k3kControlPlane.Name)

		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, &k3kControlPlane); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(&k3kControlPlane, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		if err := patchHelper.Patch(ctx, &k3kControlPlane, patch.WithOwnedConditions{Conditions: []string{
			clusterv1beta2.PausedCondition,
			controlplanev1.K3kControlPlaneAvailableCondition,
			controlplanev1.K3kControllerUpToDateCondition,
		}}); err != nil {
			reterr = errors.Join(reterr, err)
		}
	}()

	scope := &scope{
		Logger:          log,
		k3kControlPlane: &k3kControlPlane,
		cluster:         cluster,
		hostConfig:      hostConfig,
	}

	if !k3kControlPlane.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, scope)
	}

	return r.reconcileNormal(ctx, scope)
}

func (r *K3kControlPlaneReconciler) reconcileNormal(ctx context.Context, scope *scope) (ctrl.Result, error) {
	// add the finalizer before creating the upstream cluster, so it can't be left behind
	if controllerutil.AddFinalizer(scope.k3kControlPlane, finalizer) {
		return ctrl.Result{}, nil
	}

	plan, err := r.ensureK3kController(ctx, scope.hostConfig)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error installing K3k controller: %w", err)
	}

	setK3kControllerUpToDateCondition(scope.k3kControlPlane, plan)

	spec := desiredUpstreamSpec(scope.k3kControlPlane, scope.cluster)

	upstreamCluster, err := r.reconcileUpstreamCluster(ctx, scope.k3kControlPlane, spec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to reconcile k3k cluster: %w", err)
	}
	if upstreamCluster == nil {
		return ctrl.Result{}, fmt.Errorf("controlplane wasn't deleting, but we didn't reconcile the cluster")
	}

	scope.Info("Reconciled upstream cluster for controlPlane", "upstreamCluster", upstreamCluster, "controlPlane", scope.k3kControlPlane.Name)

	scope.k3kControlPlane.Status.ExternalManagedControlPlane = true
	scope.k3kControlPlane.Status.ClusterStatus = *upstreamCluster.Status.DeepCopy()
	setAvailableCondition(scope.k3kControlPlane, upstreamCluster)

	if isHCP(scope.k3kControlPlane) {
		if err := r.reconcileJoinToken(ctx, scope, upstreamCluster); err != nil {
			if apiError.IsNotFound(err) {
				scope.Info("Token for upstream cluster not ready yet", "clusterName", upstreamCluster.Name)
				return ctrl.Result{RequeueAfter: time.Second * 5}, nil
			}

			return ctrl.Result{}, fmt.Errorf("unable to reconcile join token: %w", err)
		}
	}

	config, err := r.getKubeconfig(ctx, scope, upstreamCluster)
	if err != nil {
		if apiError.IsNotFound(err) {
			scope.Info("Kubeconfig for upstream cluster not ready yet", "clusterName", upstreamCluster.Name)
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil
		}

		scope.Error(err, "failure getting kubeconfig for cluster", "clusterName", upstreamCluster.Name)

		return ctrl.Result{}, fmt.Errorf("unable to get kubeconfig secret: %w", err)
	}

	serverURL, err := serverURLFromKubeConfig(config)
	if err != nil {
		scope.Error(err, "Unable to get serverURL from kubeconfig")
		return ctrl.Result{}, fmt.Errorf("unable to resolve server url")
	}

	endpoint, err := apiEndpointFromURL(serverURL)
	if err != nil {
		scope.Error(err, "Unable to get the control plane endpoint from serverURL")
		return ctrl.Result{}, fmt.Errorf("unable to resolve control plane endpoint: %w", err)
	}

	// Worker nodes join through the Cluster endpoint: don't initialize the control plane if K3k is not exposed there.
	if clusterEndpoint := scope.cluster.Spec.ControlPlaneEndpoint; isHCP(scope.k3kControlPlane) && clusterEndpoint.IsValid() && clusterEndpoint != endpoint {
		conditions.Set(scope.k3kControlPlane, metav1.Condition{
			Type:   controlplanev1.K3kControlPlaneAvailableCondition,
			Status: metav1.ConditionFalse,
			Reason: controlplanev1.K3kControlPlaneEndpointMismatchReason,
			Message: fmt.Sprintf("K3k cluster is exposed at %s, but the Cluster control plane endpoint is %s: check the expose configuration (e.g. expose.nodePort.serverPort)",
				endpoint.String(), clusterEndpoint.String()),
		})

		return ctrl.Result{RequeueAfter: time.Second * 30}, nil
	}

	if err := r.createKubeconfigSecret(ctx, config, scope.k3kControlPlane); err != nil {
		scope.Error(err, "Unable to store kubeconfig as a secret")
		return ctrl.Result{}, fmt.Errorf("unable to store kubeconfig secret")
	}

	// CAPI copies the endpoint to the Cluster and orchestrates Machine provisioning from these fields.
	scope.k3kControlPlane.Spec.ControlPlaneEndpoint = endpoint
	scope.k3kControlPlane.Status.Initialization = &controlplanev1.K3kControlPlaneInitializationStatus{ControlPlaneInitialized: ptr.To(true)}
	scope.k3kControlPlane.Status.Version = k3kcontroller.K3SVersion(upstreamCluster)
	scope.k3kControlPlane.Status.Initialized = true
	scope.k3kControlPlane.Status.Ready = true

	// requeue until the upstream cluster is ready, to keep the status up to date
	if upstreamCluster.Status.Phase != upstream.ClusterReady {
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	return ctrl.Result{}, nil
}

func (r *K3kControlPlaneReconciler) reconcileDelete(ctx context.Context, scope *scope) (ctrl.Result, error) {
	scope.Info("About to remove upstream cluster")
	if err := r.deleteUpstreamClusters(ctx, scope.k3kControlPlane); err != nil {
		scope.Error(err, "Couldn't remove upstream cluster")
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(scope.k3kControlPlane, finalizer)

	return ctrl.Result{}, nil
}

// setAvailableCondition mirrors the readiness of the upstream K3k cluster to the Available condition.
func setAvailableCondition(controlPlane *controlplanev1.K3kControlPlane, upstreamCluster *upstream.Cluster) {
	if upstreamCluster.Status.Phase == upstream.ClusterReady {
		conditions.Set(controlPlane, metav1.Condition{
			Type:   controlplanev1.K3kControlPlaneAvailableCondition,
			Status: metav1.ConditionTrue,
			Reason: controlplanev1.K3kControlPlaneAvailableReason,
		})

		return
	}

	conditions.Set(controlPlane, metav1.Condition{
		Type:    controlplanev1.K3kControlPlaneAvailableCondition,
		Status:  metav1.ConditionFalse,
		Reason:  controlplanev1.K3kControlPlaneProvisioningReason,
		Message: fmt.Sprintf("K3k cluster %s/%s is in phase %q", upstreamCluster.Namespace, upstreamCluster.Name, upstreamCluster.Status.Phase),
	})
}

// reconcileUpstreamCluster creates/updates the k3k cluster that the k3k controllers will recognize.
// It doesn't remove clusters if the controlPlane is being deleted.
func (r *K3kControlPlaneReconciler) reconcileUpstreamCluster(ctx context.Context, controlPlane *controlplanev1.K3kControlPlane, spec upstream.ClusterSpec) (*upstream.Cluster, error) {
	log := ctrl.LoggerFrom(ctx)

	clusters, err := r.getUpstreamClusters(ctx, controlPlane)
	if err != nil {
		return nil, fmt.Errorf("failed to get current clusters for controlPlane %s/%s: %w", controlPlane.Namespace, controlPlane.Name, err)
	}

	if len(clusters) > 1 {
		var names []string
		for i := range clusters {
			names = append(names, clusters[i].Name)
		}
		joinedNames := strings.Join(names, ", ")
		log.Error(fmt.Errorf("controlplane %s owns two or more clusters: %s, refusing to process further", controlPlane.Name, joinedNames), "need to delete upstream clusters")
		if err := r.deleteUpstreamClusters(ctx, controlPlane); err != nil {
			return nil, fmt.Errorf("failed to delete upstream clusters owned by a single controPlane %s: %w", controlPlane.Name, err)
		}
		return nil, fmt.Errorf("reprovisioning needed")
	}

	if len(clusters) == 0 {
		namespace := "k3k-" + controlPlane.Name
		if controlPlane.Spec.HostTargetNamespace != "" {
			namespace = controlPlane.Spec.HostTargetNamespace
		}

		upstreamCluster := upstream.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: controlPlane.Name + "-",
				Namespace:    namespace,
				Labels: map[string]string{
					ownerNameLabel:      controlPlane.Name,
					ownerNamespaceLabel: controlPlane.Namespace,
				},
			},
			Spec: spec,
		}

		virtualClusterNamespace := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}

		if err := r.Host.Create(ctx, virtualClusterNamespace); err != nil && !apiError.IsAlreadyExists(err) {
			return nil, fmt.Errorf("unable to create namespace for controlPlane %s: %w", controlPlane.Name, err)
		}

		if err = r.Host.Create(ctx, &upstreamCluster); err != nil {
			return nil, fmt.Errorf("unable to create cluster for controlPlane %s: %w", controlPlane.Name, err)
		}

		return &upstreamCluster, nil
	}
	// at this point we have exactly one cluster
	currentCluster := clusters[0].DeepCopy()
	if reflect.DeepEqual(currentCluster.Spec, spec) {
		return currentCluster, nil
	}
	currentCluster.Spec = spec
	err = r.Host.Update(ctx, currentCluster)
	if err != nil {
		return nil, fmt.Errorf("unable to update cluster for controlPlane %s: %w", controlPlane.Name, err)
	}
	return currentCluster, nil
}

// deleteUpstreamClusters deletes all upstream clusters associated with this controlPlane.
func (r *K3kControlPlaneReconciler) deleteUpstreamClusters(ctx context.Context, controlPlane *controlplanev1.K3kControlPlane) error {
	log := ctrl.LoggerFrom(ctx)

	clusters, err := r.getUpstreamClusters(ctx, controlPlane)
	if err != nil {
		return fmt.Errorf("unable to get current clusters for controlPlane %s/%s: %w", controlPlane.Namespace, controlPlane.Name, err)
	}
	var deleteErrs []error
	for i := range clusters {
		if err := r.Host.Delete(ctx, &clusters[i]); err != nil && !apiError.IsNotFound(err) {
			log.Error(err, "failed to delete upstream cluster "+clusters[i].Name)
			deleteErrs = append(deleteErrs, err)
		}
	}
	if err := errors.Join(deleteErrs...); err != nil {
		return fmt.Errorf("failed to delete some upstream clusters: %w", err)
	}
	return nil
}

func (r *K3kControlPlaneReconciler) getUpstreamClusters(ctx context.Context, controlPlane *controlplanev1.K3kControlPlane) ([]upstream.Cluster, error) {
	var currentClusters upstream.ClusterList
	ownerNameRequirement, err := labels.NewRequirement(ownerNameLabel, selection.Equals, []string{controlPlane.Name})
	if err != nil {
		return nil, fmt.Errorf("unable to form label for controlPlane %s: %w", controlPlane.Name, err)
	}
	ownerNamespaceRequirement, err := labels.NewRequirement(ownerNamespaceLabel, selection.Equals, []string{controlPlane.Namespace})
	if err != nil {
		return nil, fmt.Errorf("unable to form label for controlPlane %s: %w", controlPlane.Name, err)
	}
	selector := labels.NewSelector().Add(*ownerNameRequirement).Add(*ownerNamespaceRequirement)
	err = r.Host.List(ctx, &currentClusters, &client.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("unable to list current clusters %w", err)
	}
	return currentClusters.Items, nil
}

// getKubeconfig retrieves the kubeconfig for the given upstream K3k cluster.
func (r *K3kControlPlaneReconciler) getKubeconfig(ctx context.Context, scope *scope, upstreamCluster *upstream.Cluster) (*clientcmdapi.Config, error) {
	// TODO: We set the expiry to one year here, but we need to continually keep this up to date
	cfg := kubeconfig.New()
	cfg.ExpiryDate = time.Hour * 24 * 365

	if isHCP(scope.k3kControlPlane) {
		if host := hcpServerHost(scope.k3kControlPlane, scope.cluster); host != "" {
			return cfg.Generate(ctx, r.Host, upstreamCluster, host)
		}
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("unable to get current config to derive hostname: %w", err)
	}
	u, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("unable to extract URL from specificed hostname: %s, %w", restConfig.Host, err)
	}

	return cfg.Generate(ctx, r.Host, upstreamCluster, u.Hostname())
}

// createKubeconfigSecret stores the kubeconfig into a secret which can be retrieved by CAPI later on
func (r *K3kControlPlaneReconciler) createKubeconfigSecret(ctx context.Context, kubeConfig *clientcmdapi.Config, controlPlane *controlplanev1.K3kControlPlane) error {
	ownerRef := controlPlaneOwnerRef(controlPlane)
	kubeConfigData, err := clientcmd.Write(*kubeConfig)
	if err != nil {
		return fmt.Errorf("unable to marshall kubeconfig %w", err)
	}
	secret := capiKubeconfig.GenerateSecretWithOwner(client.ObjectKeyFromObject(controlPlane), kubeConfigData, ownerRef)
	err = r.Create(ctx, secret)
	if apiError.IsAlreadyExists(err) {
		var currentSecret v1.Secret
		if err := r.Get(ctx, client.ObjectKeyFromObject(secret), &currentSecret); err != nil {
			return fmt.Errorf("detected conflict in secret %s, but was unable to get the secret: %w", secret.Name, err)
		}
		// TODO: We probably should correct the capi label here as well
		currentSecret = *currentSecret.DeepCopy()
		currentSecret.Data = secret.Data
		if err := r.Update(ctx, &currentSecret); err != nil {
			return fmt.Errorf("unable to update secret %s: %w", secret.Name, err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("unable to create secret %s with non-conflict error: %w", secret.Name, err)
	}
	return nil
}

// apiEndpointFromURL returns the control plane endpoint for the server URL, defaulting to the HTTPS port.
func apiEndpointFromURL(serverURL *url.URL) (clusterv1beta2.APIEndpoint, error) {
	port := int64(443)

	if p := serverURL.Port(); p != "" {
		var err error
		if port, err = strconv.ParseInt(p, 10, 32); err != nil {
			return clusterv1beta2.APIEndpoint{}, fmt.Errorf("unable to cast port %s from string to int: %w", p, err)
		}
	}

	return clusterv1beta2.APIEndpoint{
		Host: serverURL.Hostname(),
		Port: int32(port),
	}, nil
}

func serverURLFromKubeConfig(config *clientcmdapi.Config) (*url.URL, error) {
	ctx, ok := config.Contexts[config.CurrentContext]
	if !ok {
		return nil, fmt.Errorf("current context %s was not a valid context", config.CurrentContext)
	}
	cluster, ok := config.Clusters[ctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("cluster %s defined in context %s was not a valid cluster", ctx.Cluster, config.CurrentContext)
	}
	serverURL, err := url.Parse(cluster.Server)
	if err != nil {
		return nil, fmt.Errorf("unable to parse server url %s into a url", serverURL)
	}
	if serverURL == nil {
		return nil, fmt.Errorf("serverURL was parsed, but was nil")
	}
	return serverURL, nil
}

func controlPlaneOwnerRef(controlPlane *controlplanev1.K3kControlPlane) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: controlPlane.APIVersion,
		Kind:       controlPlane.Kind,
		Name:       controlPlane.Name,
		UID:        controlPlane.UID,
	}
}
