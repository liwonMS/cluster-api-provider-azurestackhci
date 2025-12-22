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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/scope"
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
)

const (
	// IPAMTimeout is the timeout for IPAM operations to ensure quick decisions
	IPAMTimeout = 5 * time.Second
	// IPAMPollInterval is how often to check IPClaim status during allocation
	IPAMPollInterval = 500 * time.Millisecond

	// Annotations for tracking IPClaim source and ownership
	AnnotationCreatedBy        = AzstackhciAPIGroup + "/created-by"
	AnnotationStaticIP         = AzstackhciAPIGroup + "/static-ip"
	AnnotationCreatedByCAPA    = AzstackhciAPIGroup + "/created-by-capa"
	AnnotationAllocationSource = AzstackhciAPIGroup + "/allocation-source"
	AllocationSourceIPAM       = "IPAM"
	AllocationSourceMOC        = "MOC"
)

type IPAddressAllocationSource string

const (
	IPAddressAllocationSourceUser IPAddressAllocationSource = "User"
	IPAddressAllocationSourceIPAM IPAddressAllocationSource = "IPAM"
	IPAddressAllocationSourceMoc  IPAddressAllocationSource = "MOC"
)

// IPAMService provides functionality to manage IPAddressClaim CRs for network interfaces
type IPAMService struct {
	client  client.Client
	logger  logr.Logger
	vmMeta  VmMeta
	nicSpec NicSpec
}

type VmMeta struct {
	clusterName string
	vmName      string
	namespace   string
	vmRef       metav1.Object
}

type NicSpec struct {
	vnetName   string
	subnetName string
}

// NewIPAMHelper creates a new IPAM helper instance with the provided client and logger
func NewIPAMHelper(vmscope *scope.VirtualMachineScope) *IPAMService {
	return &IPAMService{
		client: vmscope.Client(),
		logger: vmscope.GetLogger(),
		vmMeta: VmMeta{
			clusterName: vmscope.ClusterName(),
			vmName:      vmscope.Name(),
			namespace:   vmscope.Namespace(),
			vmRef:       vmscope.AzureStackHCIVirtualMachine,
		},
		nicSpec: NicSpec{
			vnetName:   vmscope.VnetName(),
			subnetName: vmscope.SubnetName(),
		},
	}
}

// AllocateIP tries to allocate a private IP for the given NIC using IPAM.
// If successful, it sets the allocated IP in the NIC spec.
// If fails to create the IPAllocation or retrieve the IP, it logs the error and allows MOC to handle the IP allocation.
func (h *IPAMService) AllocateIPClaim(ctx context.Context, claimName, staticIPAddress string) (string, error) {
	logger := h.logger.WithValues("AllocateVmIPClaim", h.vmMeta.vmName, "claimName", claimName)
	if _, err := h.createIPClaim(ctx, logger, claimName, staticIPAddress); err != nil {
		return "", fmt.Errorf("Failed to create IPAllocation for nic: %w", err)
	}

	allocatedIP, err := h.waitForIPAllocation(ctx, logger, claimName)
	if err != nil {
		return "", fmt.Errorf("Could not get IP from IPAllocation: %w", err)
	} else {
		return allocatedIP, nil
	}
}

// DeleteIPClaim cleans up IPClaim on failure or conflict
func (h *IPAMService) DeleteIPClaim(ctx context.Context, claimName string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	claim := &v1beta1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: h.vmMeta.namespace,
		},
	}

	if err := h.client.Delete(timeoutCtx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete IPClaim %s: %w", claimName, err)
	}

	h.logger.Info("Deleted IPClaim", "name", claimName)
	return nil
}

// SyncIPClaimAfterMOC creates IPClaim with MOC-allocated IP for tracking purposes
// This is best-effort and non-blocking, non-waiting, allocation status is not checked.
func (h *IPAMService) SyncIPClaim(ctx context.Context, claimName, mocAllocatedIP string) error {
	if mocAllocatedIP == "" {
		return nil // No IP to sync
	}

	logger := h.logger.WithValues("IPAllocationSync", h.vmMeta.vmName, "claimName", claimName)

	// Use timeout for sync operations
	syncCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	// Check if IPAllocation CR already exists
	ipAllocation := &v1beta1.IPAddressClaim{}
	err := h.client.Get(syncCtx, types.NamespacedName{Name: claimName, Namespace: h.vmMeta.namespace}, ipAllocation)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("IPAllocation is not found, creating new one", "crName", claimName)
			// Fall through to create new CR
		} else {
			return fmt.Errorf("failed to verify IPAllocation CR: %w", err)
		}
	} else {
		logger.Info("IPAllocation CR already exists", "crName", claimName)
		return nil
	}

	if _, err = h.createIPClaim(ctx, logger, claimName, mocAllocatedIP); err != nil {
		logger.Info("Failed to allocate IP from IPAM", "ipAllocation", ipAllocation.Name, "error", err)
	}

	logger.Info("Syncing completes for IPAllocation for NIC")
	return nil
}

// createIPClaim creates IPAddressClaim for static IP allocation with proper owner references
// Returns the claim name on success or error on failure
func (h *IPAMService) createIPClaim(ctx context.Context, logger logr.Logger, claimName, ip string) (string, error) {
	// Validate NIC has at least one IP configuration with a subnet
	logger.Info("createIPClaim IPAddressClaim details",
		"name", claimName,
		"namespace", h.vmMeta.namespace,
		"clusterName", h.vmMeta.clusterName,
		"ownerRef", h.vmMeta.vmRef.GetName(),
		"vnetName", h.nicSpec.vnetName,
		"allocationSource", IPAddressAllocationSourceIPAM,
	)

	claim := &v1beta1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: h.vmMeta.namespace,
			Annotations: map[string]string{
				AnnotationCreatedBy:        AnnotationCreatedByCAPA,
				AnnotationAllocationSource: AllocationSourceIPAM,
				AnnotationStaticIP:         ip,
			},
		},
		Spec: v1beta1.IPAddressClaimSpec{
			ClusterName: h.vmMeta.clusterName,
			PoolRef: corev1.TypedLocalObjectReference{
				Name:     h.resolvePoolName(),
				Kind:     "IPPool",
				APIGroup: strPtr(AzstackhciAPIGroup),
			},
		},
	}

	if err := controllerutil.SetOwnerReference(h.vmMeta.vmRef, claim, h.client.Scheme(), controllerutil.WithBlockOwnerDeletion(false)); err != nil {
		return "", fmt.Errorf("failed to set VM owner reference on IPClaim: %w", err)
	}

	if err := h.client.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			h.logger.Info("IPClaim already exists", "name", claimName)
			return claimName, nil
		}
		return "", fmt.Errorf("failed to create IPClaim %s: %w", claimName, err)
	}

	logger.Info("Created IPAddressClaim", "claim", fmt.Sprintf("%+v", claim))
	return claimName, nil
}

// WaitForIPAllocation waits for IPClaim to be fulfilled within the timeout period
// Returns the allocated IP address or error on failure/timeout
func (h *IPAMService) waitForIPAllocation(ctx context.Context, logger logr.Logger, claimName string) (string, error) {
	logger.Info("Attempting to retrieve IP from ipAddressClaim", "ipAddressClaim", claimName)
	ticker := time.NewTicker(IPAMPollInterval)
	defer ticker.Stop()

	timeoutCtx, cancel := context.WithTimeout(ctx, IPAMTimeout)
	defer cancel()

	namespacedName := types.NamespacedName{Name: claimName, Namespace: h.vmMeta.namespace}

	for {
		claim := &v1beta1.IPAddressClaim{}
		if err := h.client.Get(timeoutCtx, namespacedName, claim); err != nil {
			return "", fmt.Errorf("failed to get IPClaim: %w", err)
		}

		// Check if address is allocated
		if claim.Status.AddressRef.Name != "" {
			// Get the actual IP from IPAddress resource
			ipAddr := &v1beta1.IPAddress{}
			ipNamespacedName := types.NamespacedName{
				Name:      claim.Status.AddressRef.Name,
				Namespace: h.vmMeta.namespace,
			}

			if err := h.client.Get(timeoutCtx, ipNamespacedName, ipAddr); err != nil {
				return "", fmt.Errorf("failed to get IPAddress: %w", err)
			}

			logger.Info("IPAM allocation successful",
				"claim", claimName, "ip", ipAddr.Spec.Address)
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
func (h *IPAMService) GenerateIPClaimName(nicName string, index int) string {
	return fmt.Sprintf("ipclaim-%s-%d", nicName, index)
}

// resolvePoolName maps VNet/Subnet to IPPool name based on naming convention
func (h *IPAMService) resolvePoolName() string {
	// This follows the naming convention from azstackhci-operator
	return fmt.Sprintf("ippool-%s-0", h.nicSpec.vnetName)
}
