/*
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
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/moc-sdk-for-go/services/network"
	"github.com/microsoft/moc-sdk-for-go/services/network/virtualnetwork"
	"github.com/microsoft/moc/pkg/auth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// =============================================================================
// Constants
// =============================================================================

const (
	// AzstackhciAPIGroup is the API group for Azure Stack HCI infrastructure
	AzstackhciAPIGroup = "infrastructure.azstackhci.microsoft.com"

	// ArcVMLnetMocResourceGroup is the MOC resource group for Arc VM logical networks
	ArcVMLnetMocResourceGroup = "Default_Group"

	// ManagementVnetName is the name of the management VNet (skip IPAM for this)
	ManagementVnetName = "vnet-arcbridge"

	// IPClaim annotations
	AnnotationIPClaimCreatedBy       = AzstackhciAPIGroup + "/created-by"
	AnnotationIPClaimStaticIP        = "ipam." + AzstackhciAPIGroup + "/requested-ip"
	AnnotationIPClaimControlPlaneVIP = "ipam." + AzstackhciAPIGroup + "/control-plane-vip"
	AnnotationLogicalNetworkName     = "ipam." + AzstackhciAPIGroup + "/logicalNetworkName"
	AnnotationSubnetName             = "ipam." + AzstackhciAPIGroup + "/subnetName"
	AnnotationAllocationSource       = "ipam." + AzstackhciAPIGroup + "/allocation-source"

	// Allocation source values - indicates whether IP was allocated by IPAM operator or MOC IPAM
	AllocationSourceOperatorIPAM = "operator-ipam" // IP was allocated directly by IPAM operator
	AllocationSourceMOCIPAM      = "moc-ipam"      // IP was allocated by MOC IPAM, then synced for tracking

	// Creator identifiers for tracking which component created the claim
	IPClaimCreatorCAPH       = "caph"
	IPClaimCreatorCloudOp    = "cloud-operator"
	IPClaimCreatorAzstackhci = "azstackhci-operator"

	// IPClaimPollInterval is how often to check IPAddressClaim status
	IPClaimPollInterval = 100 * time.Millisecond

	// IPClaimTimeout is the timeout for IPClaim operations
	IPClaimTimeout = 30 * time.Second

	// ReadyConditionType is the condition type for ready status (matches clusterv1.ReadyCondition)
	ReadyConditionType = "Ready"
)

// =============================================================================
// Telemetry Interface and Implementations
// =============================================================================

// IPAMOperation represents the type of IPAM operation for telemetry
type IPAMOperation string

const (
	OperationCreate IPAMOperation = "Create"
	OperationDelete IPAMOperation = "Delete"
	OperationSync   IPAMOperation = "Sync"
	OperationGet    IPAMOperation = "Get"
)

// IPAMTelemetryWriter is an interface that consumers (CAPH, cloud-operator) implement
// to write telemetry logs in their preferred format.
type IPAMTelemetryWriter interface {
	WriteIPAMOperationLog(logger logr.Logger, operation IPAMOperation, claimName string, params map[string]string, err error)
}

// noOpTelemetryWriter is a telemetry writer that does nothing.
type noOpTelemetryWriter struct{}

func (w *noOpTelemetryWriter) WriteIPAMOperationLog(_ logr.Logger, _ IPAMOperation, _ string, _ map[string]string, _ error) {
}

// =============================================================================
// IPAMService - Main Entry Point
// =============================================================================

// IPAMServiceConfig contains configuration for creating an IPAMService.
type IPAMServiceConfig struct {
	// Required fields
	Client    client.Client
	Logger    logr.Logger
	Namespace string
	VnetName  string
	Owner     client.Object // The k8s object that owns the IP claims (e.g., VM, Cluster CR)

	// Optional MOC connection fields (required for VNet IPAM check)
	CloudFqdn  string
	Authorizer auth.Authorizer

	// Optional telemetry configuration - if nil, no-op telemetry is used
	TelemetryWriter IPAMTelemetryWriter

	// Optional fields for IP claim creation
	ClusterName string
	CreatorID   string // e.g., IPClaimCreatorCAPH, IPClaimCreatorCloudOp

	// Optional extra annotations to add to all IP claims
	ExtraAnnotations map[string]string
}

// IPAMService provides high-level IPAM operations with built-in telemetry support.
// This is the main class that CAPH and cloud-operator should use.
type IPAMService struct {
	client          client.Client
	telemetryWriter IPAMTelemetryWriter
	logger          logr.Logger

	// MOC connection fields for VNet IPAM check
	cloudFqdn  string
	authorizer auth.Authorizer

	namespace        string
	vnetName         string
	clusterName      string
	creatorID        string
	owner            client.Object
	extraAnnotations map[string]string
}

// NewIPAMService creates a new IPAMService with the given configuration.
func NewIPAMService(config IPAMServiceConfig) *IPAMService {
	// Use no-op telemetry if not provided
	telemetryWriter := config.TelemetryWriter
	if telemetryWriter == nil {
		telemetryWriter = &noOpTelemetryWriter{}
	}

	creatorID := config.CreatorID
	if creatorID == "" {
		creatorID = IPClaimCreatorAzstackhci
	}

	return &IPAMService{
		client:           config.Client,
		telemetryWriter:  telemetryWriter,
		logger:           config.Logger,
		cloudFqdn:        config.CloudFqdn,
		authorizer:       config.Authorizer,
		namespace:        "default",
		vnetName:         config.VnetName,
		clusterName:      config.ClusterName,
		creatorID:        creatorID,
		owner:            config.Owner,
		extraAnnotations: config.ExtraAnnotations,
	}
}

// IsIPAMEnabled checks if IPAM is enabled for the configured VNet.
func (s *IPAMService) IsIPAMEnabled(ctx context.Context, isLoadBalancerIP bool) bool {
	if s.vnetName == ManagementVnetName {
		s.logger.Info("Management VNet detected, skipping IPAM", "vnetName", s.vnetName)
		return false
	}

	if s.cloudFqdn == "" || s.authorizer == nil {
		s.logger.Info("MOC connection not configured, skipping IPAM", "vnetName", s.vnetName)
		return false
	}

	vnetsClient, err := virtualnetwork.NewVirtualNetworkClient(s.cloudFqdn, s.authorizer)
	if err != nil {
		s.logger.Error(err, "Failed to create VNet client, skipping IPAM")
		return false
	}

	vnets, err := vnetsClient.Get(ctx, ArcVMLnetMocResourceGroup, s.vnetName)
	if err != nil || vnets == nil || len(*vnets) == 0 {
		s.logger.Error(err, "Failed to get VNet from MOC, skipping IPAM", "vnetName", s.vnetName)
		return false
	}

	vnet := (*vnets)[0]
	if vnet.VirtualNetworkPropertiesFormat == nil ||
		vnet.VirtualNetworkPropertiesFormat.Subnets == nil ||
		len(*vnet.VirtualNetworkPropertiesFormat.Subnets) == 0 {
		s.logger.Info("VNet has no subnets, skipping IPAM", "vnetName", s.vnetName)
		return false
	}

	firstSubnet := (*vnet.VirtualNetworkPropertiesFormat.Subnets)[0]
	if firstSubnet.IPAllocationMethod != network.Static {
		s.logger.Info("VNet subnet not configured for Static IP allocation, skipping IPAM",
			"vnetName", s.vnetName, "allocationMethod", firstSubnet.IPAllocationMethod)
		return false
	}

	// For Nic creation, check for SDN VNet v2 - IPAM not supported in this mode
	// load balancer IPs only use SDN for V2 API version. By default, all LB requests are V1.
	// so they are never skipped.
	if !isLoadBalancerIP {
		if vnet.NetworkControllerConfig != nil && vnet.NetworkControllerConfig.IsSdnVnetV2Enabled {
			s.logger.Info("VNet SDN is enabled, skipping IPAM", "vnetName", s.vnetName)
			return false
		}
	}

	s.logger.Info("VNet configured for Static IP allocation, proceeding with IPAM", "vnetName", s.vnetName)
	return true
}

// AllocateIP allocates an IP address using IPAM.
// Returns the allocated IP address, or empty string if IPAM is not enabled.
func (s *IPAMService) AllocateIP(ctx context.Context, claimName string, staticIP string, isLoadBalancerIP bool) (string, error) {
	logger := s.logger.WithValues("operation", "AllocateIP", "claimName", claimName)

	if !s.IsIPAMEnabled(ctx, isLoadBalancerIP) {
		logger.Info("IPAM not enabled for VNet, skipping allocation")
		return "", nil
	}

	params := s.buildIPClaimParams(claimName, staticIP, AllocationSourceOperatorIPAM)

	if err := s.createIPClaim(ctx, params); err != nil {
		s.telemetryWriter.WriteIPAMOperationLog(logger, OperationCreate, claimName,
			map[string]string{"requestedIP": staticIP}, err)
		return "", fmt.Errorf("failed to create IPClaim: %w", err)
	}

	allocatedIP, err := s.waitForIPAllocation(ctx, claimName)
	if err != nil {
		s.telemetryWriter.WriteIPAMOperationLog(logger, OperationCreate, claimName,
			map[string]string{"requestedIP": staticIP}, err)
		return "", fmt.Errorf("failed to allocate IP: %w", err)
	}

	s.telemetryWriter.WriteIPAMOperationLog(logger, OperationCreate, claimName,
		map[string]string{"allocatedIP": allocatedIP, "requestedIP": staticIP}, nil)

	logger.Info("IPAM allocation successful", "ip", allocatedIP)
	return allocatedIP, nil
}

// DeleteIPClaim deletes an IPAddressClaim by name and waits for deletion to complete.
func (s *IPAMService) DeleteIPClaim(ctx context.Context, claimName string) (err error) {
	logger := s.logger.WithValues("operation", "DeleteIPClaim", "claimName", claimName)

	defer func() {
		s.telemetryWriter.WriteIPAMOperationLog(logger, OperationDelete, claimName, nil, err)
	}()

	timeoutCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	claim := &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: s.namespace,
		},
	}

	if err = s.client.Delete(timeoutCtx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete IPClaim %s: %w", claimName, err)
	}

	// Wait for deletion to complete
	if err = s.ensureIPClaimDeleted(ctx, claimName); err != nil {
		return err
	}

	logger.Info("Deleted IPClaim")
	return nil
}

// ensureIPClaimDeleted waits for an IPClaim to be fully removed.
func (s *IPAMService) ensureIPClaimDeleted(ctx context.Context, claimName string) error {
	s.logger.Info("Waiting for IPClaim to be deleted", "claimName", claimName)
	namespacedName := types.NamespacedName{Name: claimName, Namespace: s.namespace}

	pollErr := wait.PollUntilContextTimeout(ctx, IPClaimPollInterval, IPClaimTimeout, true, func(ctx context.Context) (bool, error) {
		claim := &ipamv1.IPAddressClaim{}
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

// SyncIPClaim creates/syncs an IPClaim with an externally allocated IP.
// This is best-effort and non-blocking (doesn't wait for allocation status).
func (s *IPAMService) SyncIPClaim(ctx context.Context, claimName, allocatedIP string, isLoadBalancerIP bool) error {
	logger := s.logger.WithValues("operation", "SyncIPClaim", "claimName", claimName, "ip", allocatedIP, "vnetName", s.vnetName)

	if allocatedIP == "" || s.vnetName == ManagementVnetName {
		return nil
	}

	// Use timeout for sync operations
	syncCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	// Check if IPClaim already exists
	claim := &ipamv1.IPAddressClaim{}
	err := s.client.Get(syncCtx, types.NamespacedName{Name: claimName, Namespace: s.namespace}, claim)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("IPClaim is not found, creating new one")
			// Fall through to create new CR
		} else {
			return fmt.Errorf("failed to verify IPClaim CR: %w", err)
		}
	} else {
		logger.Info("IPClaim CR already exists, verifying IP")
		if verifyErr := s.verifyAllocatedIP(ctx, claim, allocatedIP); verifyErr != nil {
			logger.Info("Allocated IP does not match expected IP, recreating IPClaim CR", "err", verifyErr.Error())
			// Delete existing CR to recreate
			if delErr := s.DeleteIPClaim(syncCtx, claimName); delErr != nil {
				s.telemetryWriter.WriteIPAMOperationLog(logger, OperationSync, claimName,
					map[string]string{"ip": allocatedIP}, delErr)
				return fmt.Errorf("failed to delete mismatched IPClaim CR: %w", delErr)
			}
		} else {
			return nil // IP matches, nothing to do
		}
	}

	// Only check with MOC if necessary as the call is expensive
	if !s.IsIPAMEnabled(ctx, isLoadBalancerIP) {
		return nil
	}

	// Just create, not waiting for completion
	// Note: If an IPClaim already existed with a mismatched IP, it was deleted above and
	// recreated here with AllocationSourceMOCIPAM, correctly reflecting that the final IP came from MOC.
	params := s.buildIPClaimParams(claimName, allocatedIP, AllocationSourceMOCIPAM)
	if err := s.createIPClaim(ctx, params); err != nil {
		s.telemetryWriter.WriteIPAMOperationLog(logger, OperationSync, claimName,
			map[string]string{"ip": allocatedIP}, err)
		return fmt.Errorf("failed to create IPClaim for sync: %w", err)
	}

	s.telemetryWriter.WriteIPAMOperationLog(logger, OperationSync, claimName,
		map[string]string{"ip": allocatedIP}, nil)
	logger.Info("Syncing completes for IPClaim")
	return nil
}

// verifyAllocatedIP checks if the IPAddress in the IPClaim matches the expected IP.
func (s *IPAMService) verifyAllocatedIP(ctx context.Context, claim *ipamv1.IPAddressClaim, expectedIP string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	if claim.Status.AddressRef.Name == "" {
		return fmt.Errorf("IPClaim has no allocated address")
	}

	ipAddr := &ipamv1.IPAddress{}
	ipNamespacedName := types.NamespacedName{
		Name:      claim.Status.AddressRef.Name,
		Namespace: s.namespace,
	}

	if err := s.client.Get(timeoutCtx, ipNamespacedName, ipAddr); err != nil {
		return fmt.Errorf("failed to get IPAddress %s: %w", claim.Status.AddressRef.Name, err)
	}

	if ipAddr.Spec.Address != expectedIP {
		return fmt.Errorf("IPClaim %s has mismatched IP: expected %s, got %s", claim.ObjectMeta.Name, expectedIP, ipAddr.Spec.Address)
	}

	return nil // IP matches
}

// GenerateNICIPClaimName creates a deterministic IPClaim name for NIC IP allocation.
func GenerateNICIPClaimName(nicName string, index int) string {
	return fmt.Sprintf("ipclaim-%s-%d", nicName, index)
}

// SetOwner updates the owner object for new IP claims.
func (s *IPAMService) SetOwner(owner client.Object) {
	s.owner = owner
}

// SetExtraAnnotations updates the extra annotations for new IP claims.
func (s *IPAMService) SetExtraAnnotations(annotations map[string]string) {
	s.extraAnnotations = annotations
}

// GetNamespace returns the configured namespace.
func (s *IPAMService) GetNamespace() string {
	return s.namespace
}

// GetVnetName returns the configured VNet name.
func (s *IPAMService) GetVnetName() string {
	return s.vnetName
}

// GetClusterName returns the configured cluster name.
func (s *IPAMService) GetClusterName() string {
	return s.clusterName
}

// =============================================================================
// Internal helpers
// =============================================================================

func (s *IPAMService) buildIPClaimParams(claimName, staticIP, allocationSource string) ipClaimParams {
	annotations := map[string]string{
		AnnotationIPClaimCreatedBy: s.creatorID,
	}
	if allocationSource != "" {
		annotations[AnnotationAllocationSource] = allocationSource
	}
	for k, v := range s.extraAnnotations {
		annotations[k] = v
	}

	return ipClaimParams{
		Name:        claimName,
		Namespace:   s.namespace,
		ClusterName: s.clusterName,
		VnetName:    s.vnetName,
		StaticIP:    staticIP,
		Annotations: annotations,
	}
}

type ipClaimParams struct {
	Name        string
	Namespace   string
	ClusterName string
	VnetName    string
	StaticIP    string
	Annotations map[string]string
}

func (s *IPAMService) createIPClaim(ctx context.Context, params ipClaimParams) error {
	logger := s.logger.WithValues("ipClaim", params.Name, "namespace", params.Namespace)

	annotations := make(map[string]string)
	for k, v := range params.Annotations {
		annotations[k] = v
	}
	if params.StaticIP != "" {
		annotations[AnnotationIPClaimStaticIP] = params.StaticIP
	}
	if params.VnetName != "" {
		annotations[AnnotationLogicalNetworkName] = params.VnetName
		annotations[AnnotationSubnetName] = params.VnetName
	}

	claim := &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        params.Name,
			Namespace:   params.Namespace,
			Annotations: annotations,
		},
		Spec: ipamv1.IPAddressClaimSpec{
			ClusterName: params.ClusterName,
		},
	}

	// Set owner reference
	if err := controllerutil.SetOwnerReference(s.owner, claim, s.client.Scheme(), controllerutil.WithBlockOwnerDeletion(false)); err != nil {
		return fmt.Errorf("failed to set owner reference on IPClaim: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	if err := s.client.Create(timeoutCtx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Info("IPClaim already exists")
			return nil
		}
		return fmt.Errorf("failed to create IPClaim %s: %w", params.Name, err)
	}

	logger.Info("Created IPAddressClaim")
	return nil
}

func (s *IPAMService) waitForIPAllocation(ctx context.Context, claimName string) (string, error) {
	logger := s.logger.WithValues("ipClaim", claimName)
	logger.Info("Waiting for IP allocation from IPClaim")

	timeoutCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	namespacedName := types.NamespacedName{Name: claimName, Namespace: s.namespace}

	var allocatedIP string
	var lastError string // Track the last issue for better error reporting

	pollErr := wait.PollUntilContextTimeout(timeoutCtx, IPClaimPollInterval, IPClaimTimeout, true, func(ctx context.Context) (bool, error) {
		claim := &ipamv1.IPAddressClaim{}
		if err := s.client.Get(ctx, namespacedName, claim); err != nil {
			// If not found, the cache may not have synced yet after create - keep polling
			if apierrors.IsNotFound(err) {
				lastError = "IPClaim not found (cache may not have synced)"
				return false, nil // Continue polling
			}
			// For other errors, stop immediately
			return false, fmt.Errorf("failed to get IPClaim: %w", err)
		}

		if claim.Status.AddressRef.Name != "" {
			ipAddr := &ipamv1.IPAddress{}
			ipNamespacedName := types.NamespacedName{
				Name:      claim.Status.AddressRef.Name,
				Namespace: s.namespace,
			}

			if err := s.client.Get(ctx, ipNamespacedName, ipAddr); err != nil {
				// IPAddress may not be in cache yet - keep polling
				if apierrors.IsNotFound(err) {
					lastError = fmt.Sprintf("IPAddress %s not found (cache may not have synced)", claim.Status.AddressRef.Name)
					return false, nil // Continue polling
				}
				return false, fmt.Errorf("failed to get IPAddress: %w", err)
			}

			allocatedIP = ipAddr.Spec.Address
			logger.Info("IPAM allocation successful", "ip", allocatedIP)
			return true, nil
		}

		// Check for failure conditions
		for _, condition := range claim.Status.Conditions {
			if condition.Type == ReadyConditionType && condition.Status == corev1.ConditionFalse {
				// This is a real failure from IPAM operator - stop polling
				return false, fmt.Errorf("IPAM allocation failed: %s", condition.Message)
			}
		}

		lastError = "IPClaim exists but has no addressRef yet (waiting for IPAM operator)"
		return false, nil // Continue polling
	})

	if pollErr != nil {
		return "", fmt.Errorf("timeout waiting for IP allocation after %v: %s: %w", IPClaimTimeout, lastError, pollErr)
	}

	return allocatedIP, nil
}

// =============================================================================
// Standalone Helper Functions
// =============================================================================

// DeleteIPClaimByName is a standalone helper to delete an IPAddressClaim by name.
// This can be used during cleanup without needing to create a full IPAMService.
func DeleteIPClaimByName(ctx context.Context, k8sClient client.Client, claimName, namespace string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, IPClaimTimeout)
	defer cancel()

	claim := &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: namespace,
		},
	}

	if err := k8sClient.Delete(timeoutCtx, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete IPClaim %s: %w", claimName, err)
	}

	return nil
}
