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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	// K3kClusterReadyCondition is true when the K3kCluster is provisioned.
	// K3kCluster has no infrastructure of its own to provision, so it is ready as soon as it is reconciled.
	K3kClusterReadyCondition = clusterv1beta2.ReadyCondition

	// K3kClusterReadyReason surfaces when the K3kCluster is provisioned.
	K3kClusterReadyReason = clusterv1beta2.ReadyReason
)

// K3kClusterSpec defines the desired state of K3kCluster
type K3kClusterSpec struct {
	// ControlPlaneEndpoint is the endpoint that the server can be reached at. Should not be supplied at create
	// time by the end user, the controller will fill this in when provisioning is complete.
	// +optional
	ControlPlaneEndpoint clusterv1beta2.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`
}

// K3kClusterStatus defines the observed state of K3kCluster.
type K3kClusterStatus struct {
	// Conditions represents the observations of the K3kCluster's current state.
	// Known condition types are Ready and Paused.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Initialization provides observations of the K3kCluster initialization process.
	// NOTE: Fields in this struct are part of the Cluster API contract and are used to orchestrate initial Machine provisioning.
	//
	// +optional
	Initialization *K3kClusterInitializationStatus `json:"initialization,omitempty"`

	// Ready is true when the cluster is ready to be used.
	//
	// Kept for compatibility with the v1beta1 Cluster API contract, use initialization.provisioned instead.
	//
	// +optional
	Ready bool `json:"ready,omitempty"`
}

// K3kClusterInitializationStatus provides observations of the K3kCluster initialization process.
//
// +kubebuilder:validation:MinProperties=1
type K3kClusterInitializationStatus struct {
	// Provisioned is true when the infrastructure provider reports that the cluster's infrastructure is fully provisioned.
	// NOTE: This field is part of the Cluster API contract, and it is used to orchestrate initial Machine provisioning.
	//
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// K3kCluster is the Schema for the K3kclusters API
type K3kCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired spec.
	Spec K3kClusterSpec `json:"spec,omitempty"`
	// Status is the current status.
	Status K3kClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the set of conditions for this object.
func (c *K3kCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets conditions for this object.
func (c *K3kCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

//+kubebuilder:object:root=true

// K3kClusterList contains a list of K3kCluster
type K3kClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []K3kCluster `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &K3kCluster{}, &K3kClusterList{})
}
