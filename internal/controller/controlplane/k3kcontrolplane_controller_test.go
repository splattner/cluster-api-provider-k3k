package controlplane

import (
	"context"
	"net/url"
	"testing"

	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	upstream "github.com/rancher/k3k/pkg/apis/k3k.io/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"

	. "github.com/onsi/gomega"
)

func TestAPIEndpointFromURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected clusterv1beta2.APIEndpoint
		wantErr  bool
	}{
		{
			name:     "nodePort",
			url:      "https://172.18.0.2:31923",
			expected: clusterv1beta2.APIEndpoint{Host: "172.18.0.2", Port: 31923},
		},
		{
			name:     "ingress without port defaults to 443",
			url:      "https://k3k.example.com",
			expected: clusterv1beta2.APIEndpoint{Host: "k3k.example.com", Port: 443},
		},
		{
			name:    "port out of range",
			url:     "https://k3k.example.com:99999999999",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			serverURL, err := url.Parse(tt.url)
			g.Expect(err).ToNot(HaveOccurred())

			endpoint, err := apiEndpointFromURL(serverURL)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(endpoint).To(Equal(tt.expected))
			g.Expect(endpoint.IsValid()).To(BeTrue())
		})
	}
}

func TestClusterToK3kControlPlane(t *testing.T) {
	tests := []struct {
		name     string
		ref      clusterv1beta2.ContractVersionedObjectReference
		expected []reconcile.Request
	}{
		{
			name: "K3kControlPlane reference",
			ref: clusterv1beta2.ContractVersionedObjectReference{
				APIGroup: controlplanev1.GroupVersion.Group,
				Kind:     "K3kControlPlane",
				Name:     "my-cp",
			},
			expected: []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "ns", Name: "my-cp"}}},
		},
		{
			name: "other control plane provider",
			ref: clusterv1beta2.ContractVersionedObjectReference{
				APIGroup: "controlplane.cluster.x-k8s.io",
				Kind:     "KubeadmControlPlane",
				Name:     "my-cp",
			},
		},
		{
			name: "no control plane reference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			cluster := &clusterv1beta2.Cluster{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "my-cluster"},
				Spec:       clusterv1beta2.ClusterSpec{ControlPlaneRef: tt.ref},
			}

			g.Expect(clusterToK3kControlPlane(context.Background(), cluster)).To(Equal(tt.expected))
		})
	}
}

func TestSetAvailableCondition(t *testing.T) {
	tests := []struct {
		phase          upstream.ClusterPhase
		expectedStatus metav1.ConditionStatus
		expectedReason string
	}{
		{
			phase:          upstream.ClusterReady,
			expectedStatus: metav1.ConditionTrue,
			expectedReason: controlplanev1.K3kControlPlaneAvailableReason,
		},
		{
			phase:          upstream.ClusterProvisioning,
			expectedStatus: metav1.ConditionFalse,
			expectedReason: controlplanev1.K3kControlPlaneProvisioningReason,
		},
		{
			phase:          "",
			expectedStatus: metav1.ConditionFalse,
			expectedReason: controlplanev1.K3kControlPlaneProvisioningReason,
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			g := NewWithT(t)

			controlPlane := &controlplanev1.K3kControlPlane{}
			upstreamCluster := &upstream.Cluster{Status: upstream.ClusterStatus{Phase: tt.phase}}

			setAvailableCondition(controlPlane, upstreamCluster)

			condition := conditions.Get(controlPlane, controlplanev1.K3kControlPlaneAvailableCondition)
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.Status).To(Equal(tt.expectedStatus))
			g.Expect(condition.Reason).To(Equal(tt.expectedReason))
		})
	}
}
