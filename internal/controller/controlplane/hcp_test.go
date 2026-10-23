package controlplane

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	upstream "github.com/rancher/k3k/pkg/apis/k3k.io/v1beta1"
	v1 "k8s.io/api/core/v1"
	apiError "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"

	. "github.com/onsi/gomega"
)

func TestDesiredUpstreamSpec(t *testing.T) {
	endpoint := clusterv1beta2.APIEndpoint{Host: "172.18.0.2", Port: 30443}

	tests := []struct {
		name               string
		spec               upstream.ClusterSpec
		endpoint           clusterv1beta2.APIEndpoint
		expectedTLSSANs    []string
		expectedServerArgs []string
	}{
		{
			name:            "shared mode is unchanged",
			spec:            upstream.ClusterSpec{Mode: upstream.SharedClusterMode, TLSSANs: []string{"example.com"}},
			endpoint:        endpoint,
			expectedTLSSANs: []string{"example.com"},
		},
		{
			name:               "hcp mode without endpoint disables the cloud controller",
			spec:               upstream.ClusterSpec{Mode: upstream.HCPClusterMode, TLSSANs: []string{"example.com"}},
			expectedTLSSANs:    []string{"example.com"},
			expectedServerArgs: []string{"--disable-cloud-controller"},
		},
		{
			name:               "hcp mode puts the endpoint host first in the TLS SANs",
			spec:               upstream.ClusterSpec{Mode: upstream.HCPClusterMode, TLSSANs: []string{"example.com", "172.18.0.2"}},
			endpoint:           endpoint,
			expectedTLSSANs:    []string{"172.18.0.2", "example.com"},
			expectedServerArgs: []string{"--disable-cloud-controller"},
		},
		{
			name:               "hcp mode keeps an explicit cloud controller setting",
			spec:               upstream.ClusterSpec{Mode: upstream.HCPClusterMode, ServerArgs: []string{"--disable-cloud-controller=false"}},
			endpoint:           endpoint,
			expectedTLSSANs:    []string{"172.18.0.2"},
			expectedServerArgs: []string{"--disable-cloud-controller=false"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			controlPlane := &controlplanev1.K3kControlPlane{Spec: controlplanev1.K3kControlPlaneSpec{ClusterSpec: tt.spec}}
			cluster := &clusterv1beta2.Cluster{Spec: clusterv1beta2.ClusterSpec{ControlPlaneEndpoint: tt.endpoint}}

			spec := desiredUpstreamSpec(controlPlane, cluster)

			g.Expect(spec.TLSSANs).To(Equal(tt.expectedTLSSANs))
			g.Expect(spec.ServerArgs).To(Equal(tt.expectedServerArgs))
			// the K3kControlPlane spec must not be modified
			g.Expect(controlPlane.Spec.ClusterSpec).To(Equal(tt.spec))
		})
	}
}

func TestHCPServerHost(t *testing.T) {
	tests := []struct {
		name     string
		tlsSANs  []string
		endpoint clusterv1beta2.APIEndpoint
		expected string
	}{
		{
			name:     "cluster endpoint",
			tlsSANs:  []string{"example.com"},
			endpoint: clusterv1beta2.APIEndpoint{Host: "172.18.0.2", Port: 30443},
			expected: "172.18.0.2",
		},
		{
			name:     "first non-loopback TLS SAN",
			tlsSANs:  []string{"localhost", "127.0.0.1", "::1", "example.com"},
			expected: "example.com",
		},
		{
			name:    "no host",
			tlsSANs: []string{"localhost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			controlPlane := &controlplanev1.K3kControlPlane{Spec: controlplanev1.K3kControlPlaneSpec{ClusterSpec: upstream.ClusterSpec{TLSSANs: tt.tlsSANs}}}
			cluster := &clusterv1beta2.Cluster{Spec: clusterv1beta2.ClusterSpec{ControlPlaneEndpoint: tt.endpoint}}

			g.Expect(hcpServerHost(controlPlane, cluster)).To(Equal(tt.expected))
		})
	}
}

func TestReconcileJoinToken(t *testing.T) {
	scheme := runtime.NewScheme()
	NewWithT(t).Expect(v1.AddToScheme(scheme)).To(Succeed())
	NewWithT(t).Expect(controlplanev1.AddToScheme(scheme)).To(Succeed())

	newScope := func() *scope {
		return &scope{
			Logger: logr.Discard(),
			k3kControlPlane: &controlplanev1.K3kControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cp", Namespace: "ns", UID: "cp-uid"},
			},
			cluster: &clusterv1beta2.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "ns"}},
		}
	}

	upstreamCluster := &upstream.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "my-cp-abcde", Namespace: "k3k-my-cp"}}

	tests := []struct {
		name            string
		upstreamCluster *upstream.Cluster
		hostObjects     []client.Object
		objects         []client.Object
		expectedToken   string
		expectNotFound  bool
	}{
		{
			name:            "copies the K3k token",
			upstreamCluster: upstreamCluster,
			hostObjects: []client.Object{&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "k3k-my-cp-abcde-token", Namespace: "k3k-my-cp"},
				Data:       map[string][]byte{"token": []byte("k3k-token")},
			}},
			expectedToken: "k3k-token",
		},
		{
			name: "copies the token of the tokenSecretRef",
			upstreamCluster: &upstream.Cluster{
				ObjectMeta: upstreamCluster.ObjectMeta,
				Spec:       upstream.ClusterSpec{TokenSecretRef: &v1.SecretReference{Name: "custom-token"}},
			},
			hostObjects: []client.Object{&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "custom-token", Namespace: "k3k-my-cp"},
				Data:       map[string][]byte{"token": []byte("custom")},
			}},
			expectedToken: "custom",
		},
		{
			name:            "updates an existing token secret",
			upstreamCluster: upstreamCluster,
			hostObjects: []client.Object{&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "k3k-my-cp-abcde-token", Namespace: "k3k-my-cp"},
				Data:       map[string][]byte{"token": []byte("k3k-token")},
			}},
			objects: []client.Object{&v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cluster-token", Namespace: "ns"},
				Type:       v1.SecretTypeOpaque,
				Data:       map[string][]byte{"value": []byte("old")},
			}},
			expectedToken: "k3k-token",
		},
		{
			name:            "token not created yet",
			upstreamCluster: upstreamCluster,
			expectNotFound:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			r := &K3kControlPlaneReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build(),
				Host:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.hostObjects...).Build(),
			}

			err := r.reconcileJoinToken(ctx, newScope(), tt.upstreamCluster)
			if tt.expectNotFound {
				g.Expect(apiError.IsNotFound(err)).To(BeTrue())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())

			var secret v1.Secret
			g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "my-cluster-token"}, &secret)).To(Succeed())
			g.Expect(secret.Data).To(Equal(map[string][]byte{"value": []byte(tt.expectedToken)}))
			g.Expect(secret.Labels).To(HaveKeyWithValue(clusterv1beta2.ClusterNameLabel, "my-cluster"))
			g.Expect(secret.OwnerReferences).To(HaveLen(1))
			g.Expect(secret.OwnerReferences[0].Name).To(Equal("my-cp"))
		})
	}
}
