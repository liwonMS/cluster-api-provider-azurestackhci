/*
Copyright 2024 Microsoft Corporation.

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

package ipam

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/microsoft/moc/pkg/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ipamlib "dev.azure.com/msazure/msk8s/_git/azstackhci-operator.git/pkg/ipam"
)

// Annotation for marking LoadBalancer IP claims
const AnnotationLegacyLoadBalancerIP = "ipam." + ipamlib.AzstackhciAPIGroup + "/legacy-loadbalancer-ip"

// IPAMServiceParams contains parameters for creating a new IPAMService
type IPAMServiceParams struct {
	Client      client.Client
	Logger      logr.Logger
	CloudFqdn   string
	Authorizer  auth.Authorizer
	ClusterName string
	Namespace   string
	VnetName    string
	Owner       client.Object
}

// IPAMService wraps ipamlib.IPAMService for CAPH legacy LB specific functionality.
// This is used to sync MOC-allocated IPs to the K8s-based IPAM for existing clusters
// that were created before the IPAM integration.
type IPAMService struct {
	*ipamlib.IPAMService
	clusterName string
}

// NewIPAMService creates a new IPAM service instance for CAPH.
func NewIPAMService(params IPAMServiceParams) (*IPAMService, error) {
	config := ipamlib.IPAMServiceConfig{
		Client:          params.Client,
		Logger:          params.Logger,
		Namespace:       params.Namespace,
		VnetName:        params.VnetName,
		CloudFqdn:       params.CloudFqdn,
		Authorizer:      params.Authorizer,
		TelemetryWriter: &CAPHTelemetryWriter{clusterName: params.ClusterName},
		ClusterName:     params.ClusterName,
		CreatorID:       ipamlib.IPClaimCreatorCAPH,
		Owner:           params.Owner,
		ExtraAnnotations: map[string]string{
			AnnotationLegacyLoadBalancerIP: "true",
		},
	}

	return &IPAMService{
		IPAMService: ipamlib.NewIPAMService(config),
		clusterName: params.ClusterName,
	}, nil
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

// CAPHTelemetryWriter implements the ipamlib.IPAMTelemetryWriter interface for CAPH.
type CAPHTelemetryWriter struct {
	clusterName string
}

// WriteIPAMOperationLog implements the IPAMTelemetryWriter interface.
// For now, this logs using the standard logger. In the future, this can be extended
// to write to Geneva metrics or other telemetry systems.
func (w *CAPHTelemetryWriter) WriteIPAMOperationLog(logger logr.Logger, operation ipamlib.IPAMOperation, claimName string, params map[string]string, err error) {
	logParams := []interface{}{
		"operation", string(operation),
		"claimName", claimName,
		"clusterName", w.clusterName,
	}
	for k, v := range params {
		logParams = append(logParams, k, v)
	}

	if err != nil {
		logParams = append(logParams, "error", err.Error())
		logger.Error(err, "IPAM operation failed", logParams...)
	} else {
		logger.Info("IPAM operation completed", logParams...)
	}
}
