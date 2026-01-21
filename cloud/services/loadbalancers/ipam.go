/*
Copyright 2024 The Kubernetes Authors.
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

package loadbalancers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	ipam "dev.azure.com/msazure/msk8s/_git/azstackhci-operator.git/pkg/ipam"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/scope"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/telemetry"
)

// Annotation for marking LoadBalancer IP claims
const AnnotationLegacyLoadBalancerIP = "ipam." + ipam.AzstackhciAPIGroup + "/legacy-loadbalancer-ip"

// CAPHTelemetryWriter implements ipam.IPAMTelemetryWriter for CAPH LoadBalancer.
type CAPHTelemetryWriter struct {
	clusterScope *scope.ClusterScope
}

// WriteIPAMOperationLog implements ipam.IPAMTelemetryWriter.
func (w *CAPHTelemetryWriter) WriteIPAMOperationLog(logger logr.Logger, operation ipam.IPAMOperation, claimName string, params map[string]string, err error) {
	var telemetryOp telemetry.Operation
	switch operation {
	case ipam.OperationCreate, ipam.OperationSync:
		telemetryOp = telemetry.Create
	case ipam.OperationDelete:
		telemetryOp = telemetry.Delete
	case ipam.OperationGet:
		telemetryOp = telemetry.Get
	default:
		telemetryOp = telemetry.Create
	}

	telemetry.WriteMocOperationLog(
		logger,
		telemetryOp,
		w.clusterScope.GetCustomResourceTypeWithName(),
		telemetry.IPAddressClaim,
		telemetry.GenerateMocResourceName(w.clusterScope.GetResourceGroup(), claimName),
		params,
		err,
	)
}

// IPAMService wraps ipam.IPAMService for CAPH LoadBalancer functionality.
type IPAMService struct {
	*ipam.IPAMService
	clusterName string
}

// NewIPAMService creates a new IPAM service instance for CAPH LoadBalancer.
func NewIPAMService(clusterScope *scope.ClusterScope, lbScope *scope.LoadBalancerScope) *IPAMService {
	logger := clusterScope.GetLogger()

	config := ipam.IPAMServiceConfig{
		Client:          clusterScope.Client,
		Logger:          logger,
		Namespace:       clusterScope.Namespace(),
		VnetName:        clusterScope.Vnet().Name,
		CloudFqdn:       clusterScope.GetCloudAgentFqdn(),
		Authorizer:      clusterScope.GetAuthorizer(),
		TelemetryWriter: &CAPHTelemetryWriter{clusterScope: clusterScope},
		ClusterName:     clusterScope.Name(),
		CreatorID:       ipam.IPClaimCreatorCAPH,
		Owner:           lbScope.AzureStackHCILoadBalancer,
		ExtraAnnotations: map[string]string{
			AnnotationLegacyLoadBalancerIP: "true",
		},
	}

	return &IPAMService{
		IPAMService: ipam.NewIPAMService(config),
		clusterName: clusterScope.Name(),
	}
}

// generateLegacyLoadBalancerIPClaimName creates a deterministic IPClaim name for legacy LB IP sync.
func generateLegacyLoadBalancerIPClaimName(clusterName string) string {
	return fmt.Sprintf("ipclaim-%s-legacy-lb-ip", clusterName)
}

// SyncLoadBalancerIP syncs the MOC-allocated LB IP to IPAM.
// This is best-effort and non-blocking - it creates an IPClaim with a static IP annotation
// to record the allocation in the K8s-based IPAM system.
func (s *IPAMService) SyncLoadBalancerIP(ctx context.Context, mocAllocatedIP string) error {
	claimName := generateLegacyLoadBalancerIPClaimName(s.clusterName)
	return s.IPAMService.SyncIPClaim(ctx, claimName, mocAllocatedIP)
}

// DeleteLoadBalancerIPClaim deletes the legacy LB IP claim (used during cleanup).
func (s *IPAMService) DeleteLoadBalancerIPClaim(ctx context.Context) error {
	claimName := generateLegacyLoadBalancerIPClaimName(s.clusterName)
	return s.IPAMService.DeleteIPClaim(ctx, claimName)
}

// GetLoadBalancerIPClaimName returns the claim name for external use.
func (s *IPAMService) GetLoadBalancerIPClaimName() string {
	return generateLegacyLoadBalancerIPClaimName(s.clusterName)
}
