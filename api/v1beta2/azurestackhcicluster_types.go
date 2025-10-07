/*
Copyright 2020 The Kubernetes Authors.
Portions Copyright © Microsoft Corporation.

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

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	// ClusterFinalizer allows ReconcileAzureStackHCICluster to clean up Azure resources associated with AzureStackHCICluster before
	// removing it from the apiserver.
	ClusterFinalizer = "azurestackhcicluster.infrastructure.cluster.x-k8s.io"
)

// AzureStackHCIClusterSpec defines the desired state of AzureStackHCICluster
type AzureStackHCIClusterSpec struct {
	// NetworkSpec encapsulates all things related to Azure network.
	NetworkSpec NetworkSpec `json:"networkSpec,omitempty"`

	ResourceGroup string `json:"resourceGroup"`

	Location string `json:"location"`

	// AzureStackHCILoadBalancer is used to declare the AzureStackHCILoadBalancerSpec if a LoadBalancer is desired for the AzureStackHCICluster.
	AzureStackHCILoadBalancer *AzureStackHCILoadBalancerSpec `json:"azureStackHCILoadBalancer,omitempty"`

	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint"`

	// Version indicates the desired Kubernetes version of the cluster.
	Version *string `json:"version"`

	// Management is true when the cluster is a Management Cluster.
	Management bool `json:"management,omitempty"`
}

// AzureStackHCIClusterStatus defines the observed state of AzureStackHCICluster
type AzureStackHCIClusterStatus struct {
	Bastion VM `json:"bastion,omitempty"`

	// Phase represents the current phase of cluster actuation.
	// E.g. Pending, Running, Terminating, Failed etc.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions defines current service state of the AzureStackHCICluster.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Initialization provides observations of the Cluster initialization process.
	// NOTE: fields in this struct are part of the Cluster API contract and are used to orchestrate initial Cluster provisioning.
	// The value of those fields is never updated after provisioning is completed.
	// Use conditions to monitor the operational state of the Cluster's BootstrapSecret.
	// +optional
	Initialization *clusterv1.ClusterInitializationStatus `json:"initialization,omitempty"`
}

// SetTypedPhase sets the Phase field to the string representation of AzureStackHCIClusterPhase.
func (c *AzureStackHCIClusterStatus) SetTypedPhase(p AzureStackHCIClusterPhase) {
	c.Phase = string(p)
}

// GetTypedPhase attempts to parse the Phase field and return
// the typed AzureStackHCIClusterPhase representation as described in `types.go`.
func (c *AzureStackHCIClusterStatus) GetTypedPhase() AzureStackHCIClusterPhase {
	switch phase := AzureStackHCIClusterPhase(c.Phase); phase {
	case
		AzureStackHCIClusterPhasePending,
		AzureStackHCIClusterPhaseProvisioning,
		AzureStackHCIClusterPhaseProvisioned,
		AzureStackHCIClusterPhaseDeleting,
		AzureStackHCIClusterPhaseFailed:
		return phase
	default:
		return AzureStackHCIClusterPhaseUnknown
	}
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=azurestackhciclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="AzureStackHCICluster status such as Pending/Provisioning/Provisioned/Deleting/Failed"

// AzureStackHCICluster is the Schema for the azurestackhciclusters API
type AzureStackHCICluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AzureStackHCIClusterSpec   `json:"spec,omitempty"`
	Status AzureStackHCIClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the list of conditions for AzureStackHCICluster.
func (c *AzureStackHCICluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions for AzureStackHCICluster.
func (c *AzureStackHCICluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// AzureStackHCIClusterList contains a list of AzureStackHCICluster
type AzureStackHCIClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AzureStackHCICluster `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &AzureStackHCICluster{}, &AzureStackHCIClusterList{})
}
