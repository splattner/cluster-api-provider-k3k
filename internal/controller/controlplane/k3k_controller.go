package controlplane

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "k8s.io/api/core/v1"
	apiError "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"
	"github.com/rancher/cluster-api-provider-k3k/internal/helm"
)

const (
	// k3kManagedByLabel marks the K3k Helm releases installed by this provider: only those are upgraded.
	k3kManagedByLabel = "k3k.cluster.x-k8s.io/managed-by"
	k3kManagedByValue = "cluster-api-provider-k3k"

	k3kControllerUpToDate   = controlplanev1.K3kControllerUpToDateReason
	k3kControllerOutdated   = controlplanev1.K3kControllerOutdatedReason
	k3kControllerNotManaged = controlplanev1.K3kControllerNotManagedReason
)

// k3kControllerPlan describes how to reconcile the K3k controller on the host cluster,
// and the state of the K3k controller once it's done.
type k3kControllerPlan struct {
	install bool
	upgrade bool

	reason  string
	message string
}

// planK3kController decides how to reconcile the K3k controller, from its Helm release (nil if it doesn't exist) and
// whether the K3k namespace exists. K3k is installed if missing, and the Helm release is upgraded to the required
// version only if it was installed by this provider. K3k is never downgraded.
func planK3kController(rel *release.Release, namespaceExists bool, requiredVersion string) (k3kControllerPlan, error) {
	required, err := semver.NewVersion(requiredVersion)
	if err != nil {
		return k3kControllerPlan{}, fmt.Errorf("invalid required K3k version %q: %w", requiredVersion, err)
	}

	if rel == nil {
		if namespaceExists {
			return k3kControllerPlan{
				reason: k3kControllerNotManaged,
				message: fmt.Sprintf("K3k is installed without the %s Helm release in the %s namespace, its version can't be checked: K3k %s or newer is required",
					helm.K3kReleaseName, helm.K3kNamespace, required),
			}, nil
		}

		return k3kControllerPlan{
			install: true,
			reason:  k3kControllerUpToDate,
			message: fmt.Sprintf("K3k %s installed", required),
		}, nil
	}

	if rel.Chart == nil || rel.Chart.Metadata == nil {
		return k3kControllerPlan{}, fmt.Errorf("the %s Helm release has no chart metadata", rel.Name)
	}

	installed, err := semver.NewVersion(rel.Chart.Metadata.Version)
	if err != nil {
		return k3kControllerPlan{}, fmt.Errorf("invalid K3k chart version %q: %w", rel.Chart.Metadata.Version, err)
	}

	switch {
	case !installed.LessThan(required):
		return k3kControllerPlan{
			reason:  k3kControllerUpToDate,
			message: fmt.Sprintf("K3k %s installed", installed),
		}, nil

	case isProviderRelease(rel):
		return k3kControllerPlan{
			upgrade: true,
			reason:  k3kControllerUpToDate,
			message: fmt.Sprintf("K3k upgraded from %s to %s", installed, required),
		}, nil

	default:
		return k3kControllerPlan{
			reason: k3kControllerOutdated,
			message: fmt.Sprintf("K3k %s is older than the required %s: the %s Helm release was not installed by this provider, upgrade it",
				installed, required, rel.Name),
		}, nil
	}
}

// isProviderRelease returns true if the K3k Helm release was installed by this provider. Earlier versions of the
// provider didn't label the release, but always installed it without values: such a release is adopted.
func isProviderRelease(rel *release.Release) bool {
	if managedBy, ok := rel.Labels[k3kManagedByLabel]; ok {
		return managedBy == k3kManagedByValue
	}

	return len(rel.Config) == 0
}

// ensureK3kController installs the K3k controller on the host cluster if missing, and upgrades it if it was installed
// by this provider and it's older than the required version. hostConfig is nil for the management cluster.
func (r *K3kControlPlaneReconciler) ensureK3kController(ctx context.Context, hostConfig *rest.Config) (k3kControllerPlan, error) {
	log := ctrl.LoggerFrom(ctx)

	helmClient := r.Helm

	if hostConfig != nil {
		restClientGetter, err := helm.NewRESTClientGetter(hostConfig, nil)
		if err != nil {
			return k3kControllerPlan{}, fmt.Errorf("failed to set up REST client getter for the host cluster: %w", err)
		}

		helmClient = r.Helm.WithRESTClientGetter(restClientGetter)
	}

	rel, err := helmClient.GetRelease(ctx)
	if err != nil {
		return k3kControllerPlan{}, fmt.Errorf("failed to get the K3k Helm release: %w", err)
	}

	namespaceExists := true

	if err := r.Host.Get(ctx, client.ObjectKey{Name: helm.K3kNamespace}, &v1.Namespace{}); err != nil {
		if !apiError.IsNotFound(err) {
			return k3kControllerPlan{}, fmt.Errorf("unable to get %s namespace: %w", helm.K3kNamespace, err)
		}

		namespaceExists = false
	}

	plan, err := planK3kController(rel, namespaceExists, helm.K3kChartVersion)
	if err != nil {
		return k3kControllerPlan{}, err
	}

	labels := map[string]string{k3kManagedByLabel: k3kManagedByValue}

	switch {
	case plan.install:
		log.Info("Installing K3k", "version", helm.K3kChartVersion)

		if err := helmClient.InstallChart(ctx, nil, labels); err != nil {
			return k3kControllerPlan{}, fmt.Errorf("failed to install a K3k release: %w", err)
		}

	case plan.upgrade:
		log.Info("Upgrading K3k", "version", helm.K3kChartVersion)

		if err := helmClient.UpgradeChart(ctx, labels); err != nil {
			return k3kControllerPlan{}, fmt.Errorf("failed to upgrade the K3k release: %w", err)
		}
	}

	if plan.reason == k3kControllerUpToDate {
		log.V(1).Info(plan.message)
	} else {
		log.Info(plan.message)
	}

	return plan, nil
}

// setK3kControllerUpToDateCondition reports the state of the K3k controller on the host cluster.
func setK3kControllerUpToDateCondition(controlPlane *controlplanev1.K3kControlPlane, plan k3kControllerPlan) {
	status := metav1.ConditionTrue

	switch plan.reason {
	case k3kControllerOutdated:
		status = metav1.ConditionFalse
	case k3kControllerNotManaged:
		status = metav1.ConditionUnknown
	}

	conditions.Set(controlPlane, metav1.Condition{
		Type:    controlplanev1.K3kControllerUpToDateCondition,
		Status:  status,
		Reason:  plan.reason,
		Message: plan.message,
	})
}
