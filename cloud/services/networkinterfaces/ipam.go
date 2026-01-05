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
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/moc-sdk-for-go/services/network"
	"github.com/microsoft/moc-sdk-for-go/services/network/virtualnetwork"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/scope"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/telemetry"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// strPtr returns a pointer to the given string.
func strPtr(s string) *string {
	return &s
}

type IPAddressAllocationType string

const (
	IPAddressAllocationTypeDynamic IPAddressAllocationType = "Dynamic"
	IPAddressAllocationTypeStatic  IPAddressAllocationType = "Static"
	AzstackhciAPIGroup             string                  = "infrastructure.azstackhci.microsoft.com"
	ArcVMLnetMocResourceGroup      string                  = "Default_Group"
	ManagementVnetName             string                  = "vnet-arcbridge"
)

const (
	// IPAMTimeout is the timeout for IPAM operations to ensure quick decisions
	IPAMTimeout = 5 * time.Second
	// IPAMPollInterval is how often to check IPClaim status during allocation
	IPAMPollInterval = 100 * time.Millisecond

	// Annotations for tracking IPClaim source and ownership
	AnnotationCreatedBy     = AzstackhciAPIGroup + "/created-by"
	AnnotationCreatedByCAPH = "caph"

	AnnotationStaticIP = "ipam." + AzstackhciAPIGroup + "/requested-ip"
)

// IPAMService provides functionality to manage IPAddressClaim CRs for network interfaces
type IPAMService struct {
	client      client.Client
	logger      logr.Logger
	vmScope     *scope.VirtualMachineScope
	vnetsClient virtualnetwork.VirtualNetworkClient
}

// NewIPAMService creates a new IPAM helper instance with the provided client and logger
func NewIPAMService(vmscope *scope.VirtualMachineScope) *IPAMService {
	vnetsClient, _ := virtualnetwork.NewVirtualNetworkClient(vmscope.CloudAgentFqdn, vmscope.Authorizer)
	return &IPAMService{
		client:      vmscope.Client(),
		logger:      vmscope.GetLogger(),
		vmScope:     vmscope,
		vnetsClient: *vnetsClient,
	}
}

// isIPAMEnabledForVnet checks if the VNet is configured for Static IP allocation.
// Returns true if IPAM should be used, false otherwise.
// IPAM service specifically applies to lnets created by Arc VM extension,
// which corresponds to MOC Default_Group resource group.
// the function can be replaced by passing down vnet properties through capi in the future.
func (s *IPAMService) isIPAMEnabledForVnet(ctx context.Context) bool {
	if s.vmScope.VnetName() == ManagementVnetName {
		s.logger.Info("Management VNet detected, skipping IPAM", "vnetName", s.vmScope.VnetName())
		return false
	}
	vnets, err := s.vnetsClient.Get(ctx, ArcVMLnetMocResourceGroup, s.vmScope.VnetName())
	if err != nil || vnets == nil || len(*vnets) == 0 {
		s.logger.Error(err, "Failed to get VNet from MOC", "vnetName", s.vmScope.VnetName())
		return false
	}

	vnet := (*vnets)[0]
	if vnet.VirtualNetworkPropertiesFormat == nil ||
		vnet.VirtualNetworkPropertiesFormat.Subnets == nil ||
		len(*vnet.VirtualNetworkPropertiesFormat.Subnets) == 0 {
		s.logger.Info("VNet has no subnets, skipping IPAM", "vnetName", s.vmScope.VnetName())
		return false
	}

	// Check if the first subnet is configured for static IP allocation
	firstSubnet := (*vnet.VirtualNetworkPropertiesFormat.Subnets)[0]
	if firstSubnet.IPAllocationMethod != network.Static {
		s.logger.Info("VNet subnet not configured for Static IP allocation, skipping IPAM",
			"vnetName", s.vmScope.VnetName(),
			"allocationMethod", firstSubnet.IPAllocationMethod)
		return false
	}

	s.logger.Info("VNet configured for Static IP allocation, proceeding with IPAM",
		"vnetName", s.vmScope.VnetName())
	return true
}

// AllocateIP tries to allocate a private IP for the given NIC using IPAM.
// If successful, it sets the allocated IP in the NIC spec.
// If fails to create the IPAllocation or retrieve the IP, it logs the error and allows MOC to handle the IP allocation.
func (s *IPAMService) AllocateIPClaim(ctx context.Context, claimName, staticIPAddress string) (string, error) {
	logger := s.logger.WithValues("AllocateVmIPClaim", s.vmScope.Name(), "claimName", claimName)
	if enabled := s.isIPAMEnabledForVnet(ctx); !enabled {
		return "", nil
	}

	if err := s.createIPClaim(ctx, logger, claimName, staticIPAddress); err != nil {
		return "", fmt.Errorf("Failed to create IPAllocation for nic: %w", err)
	}

	allocatedIP, err := s.waitForIPAllocation(ctx, logger, claimName)
	if err != nil {
		return "", fmt.Errorf("Could not get IP from IPAllocation: %w", err)
	}

	return allocatedIP, nil
}

// DeleteIPClaim cleans up IPClaim on failure or conflict
func (s *IPAMService) DeleteIPClaim(ctx context.Context, claimName string) (err error) {
	defer func() {
		telemetry.WriteMocOperationLog(s.logger, telemetry.Delete, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
	}()

	timeoutCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	claim := &v1beta1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: s.vmScope.Namespace(),
		},
	}

	if err := s.client.Delete(timeoutCtx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete IPClaim %s: %w", claimName, err)
	}

	if err := s.ensureIPClaimDeleted(ctx, claimName); err != nil {
		return err
	}

	s.logger.Info("Deleted IPClaim", "name", claimName)
	return nil
}

func (s *IPAMService) ensureIPClaimDeleted(ctx context.Context, claimName string) error {
	namespacedName := types.NamespacedName{Name: claimName, Namespace: s.vmScope.Namespace()}

	pollErr := wait.PollUntilContextTimeout(ctx, IPAMPollInterval, IPAMTimeout, true, func(ctx context.Context) (bool, error) {
		claim := &v1beta1.IPAddressClaim{}
		err := s.client.Get(ctx, namespacedName, claim)
		if apierrors.IsNotFound(err) {
			return true, nil // Deletion complete
		}
		if err != nil {
			return false, err
		}
		return false, nil // Continue polling
	})

	if pollErr != nil {
		return fmt.Errorf("failed waiting for IPClaim %s to be deleted: %w", claimName, pollErr)
	}

	return nil
}

// SyncIPClaimAfterMOC creates IPClaim with MOC-allocated IP for tracking purposes
// This is best-effort and non-blocking, non-waiting, allocation status is not checked.
func (s *IPAMService) SyncIPClaim(ctx context.Context, claimName, mocAllocatedIP string) error {
	logger := s.logger.WithValues("IPAllocationSync", s.vmScope.Name(), "claimName", claimName, "mocAllocatedIP", mocAllocatedIP, "vnetName", s.vmScope.VnetName())
	if mocAllocatedIP == "" || s.vmScope.VnetName() == ManagementVnetName {
		return nil // No IP to sync
	}

	// Use timeout for sync operations
	syncCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	// Check if IPAllocation CR already exists
	ipClaim := &v1beta1.IPAddressClaim{}
	err := s.client.Get(syncCtx, types.NamespacedName{Name: claimName, Namespace: s.vmScope.Namespace()}, ipClaim)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("IPAllocation is not found, creating new one")
			// Fall through to create new CR
		} else {
			return fmt.Errorf("failed to verify IPAllocation CR: %w", err)
		}
	} else {
		logger.Info("IPAllocation CR already exists, verifying IP")
		if err := s.verifyAllocatedIP(ctx, ipClaim, mocAllocatedIP); err != nil {
			logger.Info("Allocated IP does not match expected MOC IP, recreating IPAllocation CR", "err", err.Error())
			// Delete existing CR to recreate
			if delErr := s.DeleteIPClaim(syncCtx, claimName); delErr != nil {
				return fmt.Errorf("failed to delete mismatched IPAllocation CR: %w", delErr)
			}
		} else {
			return nil // IP matches, nothing to do
		}
	}

	// only check with moc if necessary as the call is expensive.
	if enabled := s.isIPAMEnabledForVnet(ctx); !enabled {
		return nil
	}

	// just create, not waiting for completion
	if err = s.createIPClaim(ctx, logger, claimName, mocAllocatedIP); err != nil {
		return fmt.Errorf("Failed to allocate IP from IPAM: %w", err)
	}

	logger.Info("Syncing completes for IPAllocation for NIC")
	return nil
}

// VerifyAllocatedIP checks if the IPAddress in the IPClaim matches the expected IP
func (s *IPAMService) verifyAllocatedIP(ctx context.Context, claim *v1beta1.IPAddressClaim, expectedIP string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	if claim.Status.AddressRef.Name == "" {
		return fmt.Errorf("IPClaim has no allocated address")
	}

	ipAddr := &v1beta1.IPAddress{}
	ipNamespacedName := types.NamespacedName{
		Name:      claim.Status.AddressRef.Name,
		Namespace: s.vmScope.Namespace(),
	}

	if err := s.client.Get(timeoutCtx, ipNamespacedName, ipAddr); err != nil {
		return fmt.Errorf("failed to get IPAddress %s: %w", claim.Status.AddressRef.Name, err)
	}

	if ipAddr.Spec.Address != expectedIP {
		return fmt.Errorf("IPClaim %s has mismatched IP: expected %s, got %s", claim.ObjectMeta.Name, expectedIP, ipAddr.Spec.Address)
	}

	return nil // IP matches
}

// createIPClaim creates IPAddressClaim for static IP allocation with proper owner references
// Returns the claim name on success or error on failure
func (s *IPAMService) createIPClaim(ctx context.Context, logger logr.Logger, claimName, ip string) (err error) {
	defer func() {
		telemetry.WriteMocOperationLog(logger, telemetry.Create, s.vmScope.GetCustomResourceTypeWithName(), telemetry.IPAddressClaim,
			telemetry.GenerateMocResourceName(s.vmScope.GetResourceGroup(), claimName), nil, err)
	}()

	logger.Info("createIPClaim IPAddressClaim details",
		"ip", ip,
		"namespace", s.vmScope.Namespace(),
		"clusterName", s.vmScope.ClusterName(),
		"ownerRef", s.vmScope.Name(),
		"vnetName", s.vmScope.VnetName(),
	)

	claim := &v1beta1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: s.vmScope.Namespace(),
			Annotations: map[string]string{
				AnnotationCreatedBy: AnnotationCreatedByCAPH,
				AnnotationStaticIP:  ip,
			},
		},
		Spec: v1beta1.IPAddressClaimSpec{
			ClusterName: s.vmScope.ClusterName(),
			PoolRef: corev1.TypedLocalObjectReference{
				Name:     s.resolvePoolName(),
				Kind:     "IPPool",
				APIGroup: strPtr(AzstackhciAPIGroup),
			},
		},
	}

	if err = controllerutil.SetOwnerReference(s.vmScope.AzureStackHCIVirtualMachine, claim, s.client.Scheme(), controllerutil.WithBlockOwnerDeletion(false)); err != nil {
		return fmt.Errorf("failed to set VM owner reference on IPClaim: %w", err)
	}

	if err = s.client.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			s.logger.Info("IPClaim already exists")
			return nil
		}
		return fmt.Errorf("failed to create IPClaim %s: %w", claimName, err)
	}

	logger.Info("Created IPAddressClaim", "claim", fmt.Sprintf("%+v", claim))
	return nil
}

// WaitForIPAllocation waits for IPClaim to be fulfilled within the timeout period
// Returns the allocated IP address or error on failure/timeout
func (s *IPAMService) waitForIPAllocation(ctx context.Context, logger logr.Logger, claimName string) (string, error) {
	logger.Info("Attempting to retrieve IP from ipAddressClaim")
	ticker := time.NewTicker(IPAMPollInterval)
	defer ticker.Stop()

	timeoutCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	namespacedName := types.NamespacedName{Name: claimName, Namespace: s.vmScope.Namespace()}

	for {
		claim := &v1beta1.IPAddressClaim{}
		if err := s.client.Get(timeoutCtx, namespacedName, claim); err != nil {
			return "", fmt.Errorf("failed to get IPClaim: %w", err)
		}

		// Check if address is allocated
		if claim.Status.AddressRef.Name != "" {
			// Get the actual IP from IPAddress resource
			ipAddr := &v1beta1.IPAddress{}
			ipNamespacedName := types.NamespacedName{
				Name:      claim.Status.AddressRef.Name,
				Namespace: s.vmScope.Namespace(),
			}

			if err := s.client.Get(timeoutCtx, ipNamespacedName, ipAddr); err != nil {
				return "", fmt.Errorf("failed to get IPAddress: %w", err)
			}

			logger.Info("IPAM allocation successful", "ip", ipAddr.Spec.Address)
			return ipAddr.Spec.Address, nil
		}

		// Check for failure conditions
		for _, condition := range claim.Status.Conditions {
			if condition.Type == clusterv1.ReadyCondition &&
				condition.Status == corev1.ConditionFalse {
				return "", fmt.Errorf("IPAM allocation failed: %s", condition.Message)
			}
		}

		// Wait for next poll or timeout
		select {
		case <-timeoutCtx.Done():
			return "", fmt.Errorf("timeout waiting for IP allocation after %v", IPAMTimeout)
		case <-ticker.C:
			// Continue polling
		}
	}
}

// GenerateIPClaimName creates a deterministic IPClaim CR name from NIC spec
func (s *IPAMService) GenerateIPClaimName(nicName string, index int) string {
	return fmt.Sprintf("ipclaim-%s-%d", nicName, index)
}

// resolvePoolName maps VNet/Subnet to IPPool name based on naming convention
func (s *IPAMService) resolvePoolName() string {
	// This follows the naming convention from azstackhci-operator
	// TODO: change to return this instead befor merge.
	// return fmt.Sprintf("ippool-%s-%s-0", s.vmScope.VnetName(), s.vmScope.VnetName())
	return fmt.Sprintf("ippool-%s-0", s.vmScope.VnetName())
}
