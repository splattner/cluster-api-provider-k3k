package controlplane

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	upstream "github.com/rancher/k3k/pkg/apis/k3k.io/v1beta1"
	k3kcontroller "github.com/rancher/k3k/pkg/controller"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"
)

// disableCloudControllerArg disables the K3s embedded cloud controller manager. Nodes joined in hcp mode
// must get their providerID from the infrastructure provider (or its cloud controller manager),
// otherwise Cluster API can't match Machines to Nodes.
const disableCloudControllerArg = "--disable-cloud-controller"

func isHCP(controlPlane *controlplanev1.K3kControlPlane) bool {
	return controlPlane.Spec.Mode == upstream.HCPClusterMode
}

// desiredUpstreamSpec returns the spec of the upstream K3k cluster for the K3kControlPlane.
//
// In hcp mode, the control plane endpoint of the CAPI Cluster (if set) is used as the first TLS SAN:
// K3k uses it to point the in-cluster kubernetes Service to the externally reachable endpoint.
// The K3s embedded cloud controller manager is disabled, unless configured otherwise.
func desiredUpstreamSpec(controlPlane *controlplanev1.K3kControlPlane, cluster *clusterv1beta2.Cluster) upstream.ClusterSpec {
	spec := *controlPlane.Spec.ClusterSpec.DeepCopy()

	if !isHCP(controlPlane) {
		return spec
	}

	if cluster.Spec.ControlPlaneEndpoint.IsValid() {
		host := cluster.Spec.ControlPlaneEndpoint.Host
		otherSANs := slices.DeleteFunc(slices.Clone(spec.TLSSANs), func(san string) bool { return san == host })
		spec.TLSSANs = append([]string{host}, otherSANs...)
	}

	if !slices.ContainsFunc(spec.ServerArgs, isDisableCloudControllerArg) {
		spec.ServerArgs = append(spec.ServerArgs, disableCloudControllerArg)
	}

	return spec
}

func isDisableCloudControllerArg(arg string) bool {
	return arg == disableCloudControllerArg || strings.HasPrefix(arg, disableCloudControllerArg+"=")
}

// hcpServerHost returns the externally reachable host of the K3k cluster in hcp mode: the host of the
// CAPI Cluster control plane endpoint, or the first non-loopback TLS SAN (the same one used by K3k).
func hcpServerHost(controlPlane *controlplanev1.K3kControlPlane, cluster *clusterv1beta2.Cluster) string {
	if cluster.Spec.ControlPlaneEndpoint.IsValid() {
		return cluster.Spec.ControlPlaneEndpoint.Host
	}

	for _, san := range controlPlane.Spec.TLSSANs {
		if san == "localhost" {
			continue
		}

		if ip := net.ParseIP(san); ip != nil && ip.IsLoopback() {
			continue
		}

		return san
	}

	return ""
}

// reconcileJoinToken copies the K3k cluster token to the "<cluster>-token" Secret, where the k3s
// bootstrap provider (KThreesConfig) looks it up to join worker nodes.
func (r *K3kControlPlaneReconciler) reconcileJoinToken(ctx context.Context, scope *scope, upstreamCluster *upstream.Cluster) error {
	// same name as github.com/rancher/k3k/pkg/controller/cluster.TokenSecretName
	tokenSecretName := k3kcontroller.SafeConcatNameWithPrefix(upstreamCluster.Name, "token")
	if ref := upstreamCluster.Spec.TokenSecretRef; ref != nil && ref.Name != "" {
		tokenSecretName = ref.Name
	}

	var hostSecret v1.Secret
	if err := r.Host.Get(ctx, client.ObjectKey{Namespace: upstreamCluster.Namespace, Name: tokenSecretName}, &hostSecret); err != nil {
		return err
	}

	token, ok := hostSecret.Data["token"]
	if !ok || len(token) == 0 {
		return fmt.Errorf("token secret %s/%s has no token", hostSecret.Namespace, hostSecret.Name)
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.cluster.Name + "-token",
			Namespace: scope.cluster.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}

		secret.Labels[clusterv1beta2.ClusterNameLabel] = scope.cluster.Name
		secret.Data = map[string][]byte{"value": token}

		// the type of a Secret is immutable
		if secret.CreationTimestamp.IsZero() {
			secret.Type = clusterv1beta2.ClusterSecretType
		}

		return controllerutil.SetControllerReference(scope.k3kControlPlane, secret, r.Scheme())
	})

	return err
}
