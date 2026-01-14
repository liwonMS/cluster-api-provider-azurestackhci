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

package networkinterfaces

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ipamhelper "dev.azure.com/msazure/msk8s/_git/azstackhci-operator.git/pkg/ipam"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/scope"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/telemetry"
)

// IPAMService provides functionality to manage IPAddressClaim CRs for network interfaces.
// It wraps the shared ipamhelper package from azstackhci-operator.
type IPAMService struct {
	claimHelper *ipamhelper.IPClaimHelper
	vnetChecker *ipamhelper.VNetChecker
	client      client.Client
	logger      logr.Logger
	vmScope     *scope.VirtualMachineScope
}

// NewIPAMService creates a new IPAM service instance with the provided VM scope.
// If VNetChecker creation fails, the service is still returned but IPAM will be disabled.
func NewIPAMService(vmscope *scope.VirtualMachineScope) *IPAMService {
	logger := vmscope.GetLogger()

	vnetChecker, err := ipamhelper.NewVNetChecker(vmscope.CloudAgentFqdn, vmscope.Authorizer, logger)
	if err != nil {
		// Log error but continue - IPAM will be disabled
		logger.Error(err, "Failed to create VNet checker, IPAM will be disabled")
		vnetChecker = nil
	}

	claimHelper := ipamhelper.NewIPClaimHelper(vmscope.Client(), logger)

	return &IPAMService{
		claimHelper: claimHelper,
		vnetChecker: vnetChecker,
		client:      vmscope.Client(),
		logger:      logger,
		vmScope:     vmscope,
	}
}

// AllocateIPClaim tries to allocate a private IP for the given NIC using IPAM.
// If successful, returns the allocated IP address.
// If staticIPAddress is provided, it will be used as the requested IP.
func (s *IPAMService) AllocateIPClaim(ctx context.Context, claimName, staticIPAddress string) (string, error) {
	logger := s.logger.WithValues("AllocateVmIPClaim", s.vmScope.Name(), "claimName", claimName)

	if !ipamhelper.IsIPAMEnabled(ctx, s.vnetChecker, s.vmScope.VnetName()) {
		return "", nil
	}

	params := s.buildIPClaimParams(claimName, staticIPAddress)

	allocatedIP, err := s.claimHelper.AllocateIP(ctx, params)
	if err != nil {
		telemetry.WriteMocOperationLog(logger, telemetry.Create, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
		return "", fmt.Errorf("failed to allocate IP from IPAM: %w", err)
	}

	telemetry.WriteMocOperationLog(logger, telemetry.Create, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
		telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, nil)

	logger.Info("IPAM allocation successful", "claim", claimName, "ip", allocatedIP)
	return allocatedIP, nil
}

// DeleteIPClaim cleans up IPClaim on failure or conflict.
// Returns nil if the claim doesn't exist (idempotent).
func (s *IPAMService) DeleteIPClaim(ctx context.Context, claimName string) (err error) {
	logger := s.logger.WithValues("DeleteIPClaim", claimName)

	defer func() {
		telemetry.WriteMocOperationLog(logger, telemetry.Delete, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
	}()

	return s.claimHelper.DeleteIPClaim(ctx, claimName, s.vmScope.Namespace())
}

// EnsureIPClaimDeleted deletes an IPClaim and waits for it to be fully removed.
// This is useful when recreating claims to avoid conflicts.
func (s *IPAMService) EnsureIPClaimDeleted(ctx context.Context, claimName string) error {
	logger := s.logger.WithValues("EnsureIPClaimDeleted", claimName)

	if err := s.claimHelper.EnsureIPClaimDeleted(ctx, claimName, s.vmScope.Namespace()); err != nil {
		telemetry.WriteMocOperationLog(logger, telemetry.Delete, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
		return err
	}

	telemetry.WriteMocOperationLog(logger, telemetry.Delete, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
		telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, nil)

	return nil
}

// SyncIPClaim creates IPClaim with MOC-allocated IP for tracking purposes.
// This is best-effort and non-blocking (doesn't wait for allocation status).
// If the claim exists with the same IP, it's a no-op. If the IP differs, it recreates the claim.
func (s *IPAMService) SyncIPClaim(ctx context.Context, claimName, mocAllocatedIP string) error {
	logger := s.logger.WithValues("SyncIPClaim", s.vmScope.Name(), "claimName", claimName, "mocAllocatedIP", mocAllocatedIP)

	if mocAllocatedIP == "" || s.vmScope.VnetName() == ipamhelper.ManagementVnetName {
		return nil // No IP to sync
	}

	if !ipamhelper.IsIPAMEnabled(ctx, s.vnetChecker, s.vmScope.VnetName()) {
		return nil
	}

	params := s.buildIPClaimParams(claimName, mocAllocatedIP)

	if err := s.claimHelper.SyncIPClaim(ctx, params); err != nil {
		telemetry.WriteMocOperationLog(logger, telemetry.Create, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
		return fmt.Errorf("failed to sync IPClaim: %w", err)
	}

	telemetry.WriteMocOperationLog(logger, telemetry.Create, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
		telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, nil)

	logger.Info("IPClaim synced successfully")
	return nil
}

// VerifyIPClaimAddress checks if the IPAddress in the IPClaim matches the expected IP.
func (s *IPAMService) VerifyIPClaimAddress(ctx context.Context, claimName, expectedIP string) error {
	return s.claimHelper.VerifyIPClaimAddress(ctx, claimName, s.vmScope.Namespace(), expectedIP)
}

// GetIPClaimAddress retrieves the allocated IP from an existing IPClaim (non-blocking).
// Returns empty string if the claim doesn't exist or has no allocated address yet.
func (s *IPAMService) GetIPClaimAddress(ctx context.Context, claimName string) (string, error) {
	return s.claimHelper.GetIPClaimAddress(ctx, claimName, s.vmScope.Namespace())
}

// GenerateIPClaimName creates a deterministic IPClaim CR name from NIC spec.
// Format: ipclaim-<nicName>-<index>
func (s *IPAMService) GenerateIPClaimName(nicName string, index int) string {
	return ipamhelper.GenerateNICIPClaimName(nicName, index)
}

// buildIPClaimParams creates IPClaimParams with proper owner references for NIC IP allocation.
func (s *IPAMService) buildIPClaimParams(claimName, staticIP string) ipamhelper.IPClaimParams {
	var ownerRefs []metav1.OwnerReference

	// Set owner reference to the AzureStackHCIVirtualMachine CR
	if s.vmScope.AzureStackHCIVirtualMachine != nil {
		tempMeta := &metav1.ObjectMeta{}
		if err := controllerutil.SetOwnerReference(s.vmScope.AzureStackHCIVirtualMachine, tempMeta, s.client.Scheme(), controllerutil.WithBlockOwnerDeletion(false)); err == nil {
			if len(tempMeta.OwnerReferences) > 0 {
				ownerRefs = []metav1.OwnerReference{tempMeta.OwnerReferences[0]}
			}
		}
	}

	return ipamhelper.BuildIPClaimParams(
		claimName,
		s.vmScope.Namespace(),
		s.vmScope.ClusterName(),
		s.vmScope.VnetName(),
		staticIP,
		ipamhelper.IPClaimCreatorCAPH,
		ownerRefs,
		nil,
	)
}

// DeleteIPClaimByName is a standalone helper function to delete an IPAddressClaim by name.
// This can be used during cleanup without needing to create a full IPAMService.
// Returns nil if the claim doesn't exist (NotFound is not an error).
func DeleteIPClaimByName(ctx context.Context, k8sClient client.Client, claimName, namespace string) error {
	claimHelper := ipamhelper.NewIPClaimHelper(k8sClient, logr.Discard())
	return claimHelper.DeleteIPClaim(ctx, claimName, namespace)
}
