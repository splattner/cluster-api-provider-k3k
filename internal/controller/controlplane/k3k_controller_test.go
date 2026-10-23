package controlplane

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"sigs.k8s.io/cluster-api/util/conditions"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	controlplanev1 "github.com/rancher/cluster-api-provider-k3k/api/controlplane/v1beta1"

	. "github.com/onsi/gomega"
)

func TestPlanK3kController(t *testing.T) {
	newRelease := func(version string, labels map[string]string, config map[string]any) *release.Release {
		return &release.Release{
			Name:   "k3k",
			Chart:  &chart.Chart{Metadata: &chart.Metadata{Name: "k3k", Version: version}},
			Labels: labels,
			Config: config,
		}
	}

	providerLabels := map[string]string{k3kManagedByLabel: k3kManagedByValue}

	tests := []struct {
		name            string
		release         *release.Release
		namespaceExists bool
		expectInstall   bool
		expectUpgrade   bool
		expectedReason  string
	}{
		{
			name:           "not installed",
			expectInstall:  true,
			expectedReason: k3kControllerUpToDate,
		},
		{
			name:            "installed without the Helm release",
			namespaceExists: true,
			expectedReason:  k3kControllerNotManaged,
		},
		{
			name:            "up to date",
			release:         newRelease("1.2.0", providerLabels, nil),
			namespaceExists: true,
			expectedReason:  k3kControllerUpToDate,
		},
		{
			name:            "newer version is not downgraded",
			release:         newRelease("1.3.0", providerLabels, nil),
			namespaceExists: true,
			expectedReason:  k3kControllerUpToDate,
		},
		{
			name:            "outdated provider release is upgraded",
			release:         newRelease("1.1.0", providerLabels, nil),
			namespaceExists: true,
			expectUpgrade:   true,
			expectedReason:  k3kControllerUpToDate,
		},
		{
			name:            "outdated unlabeled release without values is adopted",
			release:         newRelease("1.1.0", nil, nil),
			namespaceExists: true,
			expectUpgrade:   true,
			expectedReason:  k3kControllerUpToDate,
		},
		{
			name:            "outdated unlabeled release with values is not upgraded",
			release:         newRelease("1.1.0", nil, map[string]any{"replicaCount": 2}),
			namespaceExists: true,
			expectedReason:  k3kControllerOutdated,
		},
		{
			name:            "outdated release managed by someone else is not upgraded",
			release:         newRelease("1.1.0", map[string]string{k3kManagedByLabel: "someone-else"}, nil),
			namespaceExists: true,
			expectedReason:  k3kControllerOutdated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			plan, err := planK3kController(tt.release, tt.namespaceExists, "1.2.0")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(plan.install).To(Equal(tt.expectInstall))
			g.Expect(plan.upgrade).To(Equal(tt.expectUpgrade))
			g.Expect(plan.reason).To(Equal(tt.expectedReason))
			g.Expect(plan.message).ToNot(BeEmpty())
		})
	}
}

func TestPlanK3kControllerInvalidVersion(t *testing.T) {
	g := NewWithT(t)

	rel := &release.Release{Name: "k3k", Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "not-a-version"}}}

	_, err := planK3kController(rel, true, "1.2.0")
	g.Expect(err).To(HaveOccurred())
}

func TestSetK3kControllerUpToDateCondition(t *testing.T) {
	tests := []struct {
		reason         string
		expectedStatus metav1.ConditionStatus
	}{
		{reason: k3kControllerUpToDate, expectedStatus: metav1.ConditionTrue},
		{reason: k3kControllerOutdated, expectedStatus: metav1.ConditionFalse},
		{reason: k3kControllerNotManaged, expectedStatus: metav1.ConditionUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			g := NewWithT(t)

			controlPlane := &controlplanev1.K3kControlPlane{}
			setK3kControllerUpToDateCondition(controlPlane, k3kControllerPlan{reason: tt.reason, message: "message"})

			condition := conditions.Get(controlPlane, controlplanev1.K3kControllerUpToDateCondition)
			g.Expect(condition).ToNot(BeNil())
			g.Expect(condition.Status).To(Equal(tt.expectedStatus))
			g.Expect(condition.Reason).To(Equal(tt.reason))
			g.Expect(condition.Message).To(Equal("message"))
		})
	}
}
