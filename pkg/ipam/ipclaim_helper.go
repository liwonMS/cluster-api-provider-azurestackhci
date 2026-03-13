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
	appsv1 "k8s.io/api/apps/v1"
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
	AnnotationIPClaimStaticIP    = "ipam." + AzstackhciAPIGroup + "/requested-ip"
	AnnotationLogicalNetworkName = "ipam." + AzstackhciAPIGroup + "/logicalNetworkName"
	AnnotationSubnetName             = "ipam." + AzstackhciAPIGroup + "/subnetName"
	AnnotationAllocationSource       = "ipam." + AzstackhciAPIGroup + "/allocation-source"

	// MOC resource annotations for tracking the underlying MOC resource associated with an IPClaim
	AnnotationMocGroupName    = AzstackhciAPIGroup + "/moc-group-name"
	AnnotationMocResourceName = AzstackhciAPIGroup + "/moc-resource-name"
	AnnotationMocResourceType = AzstackhciAPIGroup + "/moc-resource-type"

	// MOC resource type values
	MocResourceTypeNIC          = "nic"
	MocResourceTypeLoadBalancer = "load-balancer"

	// Tags used to identify ArcVM-owned virtual networks in MOC.
	// We check multiple tags for compatibility across different moc-operator versions and gantry.
	MocOperatorResourceNameTag = "MocOperatorResourceName" // Arc VM moc-operator tag
	MocOperatorNameTag         = "MocOperatorName"         // Legacy Arc VM moc-operator tag
	ArcVMOwnedTag              = "ArcVMOwned"              // Overlay-applied tag

	// Allocation source values - indicates whether IP was allocated by IPAM operator or MOC IPAM
	AllocationSourceOperatorIPAM = "ipam-operator" // IP was allocated directly by IPAM operator
	AllocationSourceMOCIPAM      = "moc-ipam"      // IP was allocated by MOC IPAM, then synced for tracking

	// Creator identifiers for tracking which component created the claim
	IPClaimCreatorCAPH    = "caph"
	IPClaimCreatorCloudOp = "cloud-operator"

	// IPClaimPollInterval is how often to check IPAddressClaim status
	IPClaimPollInterval = 100 * time.Millisecond

	// IPClaimTimeout is the timeout for IPClaim operations
	IPClaimTimeout = 30 * time.Second

	// ReadyConditionType is the condition type for ready status (matches clusterv1.ReadyCondition)
	ReadyConditionType = "Ready"

	// Deployment name and namespace for detecting azstackhci-operator presence
	azstackhciOperatorDeploymentName      = "azstackhci-operator-controller-manager"
	azstackhciOperatorDeploymentNamespace = "azstackhci-operator-system"
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

// noOpTelemetryWriter is a default IPAMTelemetryWriter that discards all telemetry.
// It is used when no custom telemetry writer is provided to IPAMServiceConfig.
type noOpTelemetryWriter struct{}

// WriteIPAMOperationLog is a no-op implementation that discards all telemetry events.
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

	namespace   string
	vnetName    string
	clusterName string
	creatorID   string
	owner       client.Object
}

// NewIPAMService creates a new IPAMService from the given configuration.
// If TelemetryWriter is nil, a no-op writer is used. The namespace defaults to "default".
func NewIPAMService(config IPAMServiceConfig) *IPAMService {
	// Use no-op telemetry if not provided
	telemetryWriter := config.TelemetryWriter
	if telemetryWriter == nil {
		telemetryWriter = &noOpTelemetryWriter{}
	}

	return &IPAMService{
		client:          config.Client,
		telemetryWriter: telemetryWriter,
		logger:          config.Logger,
		cloudFqdn:       config.CloudFqdn,
		authorizer:      config.Authorizer,
		namespace:       "default",
		vnetName:        config.VnetName,
		clusterName:     config.ClusterName,
		creatorID:       config.CreatorID,
		owner:           config.Owner,
	}
}

// arcVMOwnershipTags lists the MOC VNet tags that indicate ArcVM ownership.
// We check multiple tags for compatibility:
//   - MocOperatorResourceName: Arc VM moc-operator tag
//   - MocOperatorName: legacy Arc VM moc-operator tag from older versions
//   - ArcVMOwned: tag applied by Overlay for ArcVM-owned VNets
var arcVMOwnershipTags = []string{
	MocOperatorResourceNameTag,
	MocOperatorNameTag,
	ArcVMOwnedTag,
}

// isArcVMOwnedVNet checks whether a VNet's tags contain any recognized ArcVM ownership tag.
// Returns true if any of the known ownership tags is present with a non-empty value.
func isArcVMOwnedVNet(tags map[string]*string) bool {
	if tags == nil {
		return false
	}
	for _, tag := range arcVMOwnershipTags {
		if val, ok := tags[tag]; ok && val != nil && *val != "" {
			return true
		}
	}
	return false
}

// isIPAMAllocationEnabled determines whether IPAM allocation should proceed for the configured VNet.
// IPAM allocation is only enabled for ArcVM-owned virtual networks (identified by the MocOperatorResourceName tag)
// that are configured with static IP allocation on their first subnet.
// It returns (false, nil) when IPAM should be skipped (e.g., management VNet, not ArcVM-owned,
// no subnets, or non-static allocation).
// It returns (false, error) when the check cannot be performed due to missing MOC connection
// configuration or failure to establish a VNet client connection.
func (s *IPAMService) isIPAMAllocationEnabled(ctx context.Context) (bool, error) {
	// Check if this is the management VNet (not eligible for IPAM)
	if s.vnetName == ManagementVnetName {
		s.logger.Info("Management VNet detected, skipping IPAM", "vnetName", s.vnetName)
		return false, nil
	}

	// Check MOC connection is configured
	if s.cloudFqdn == "" || s.authorizer == nil {
		return false, fmt.Errorf("MOC connection not configured: cloudFqdn=%q, authorizerPresent=%v", s.cloudFqdn, s.authorizer != nil)
	}

	// Check VNet exists in MOC Default_Group (where Arc VM Lnets are created)
	vnetsClient, err := virtualnetwork.NewVirtualNetworkClient(s.cloudFqdn, s.authorizer)
	if err != nil {
		return false, fmt.Errorf("failed to create VNet client: %w", err)
	}

	vnets, err := vnetsClient.Get(ctx, ArcVMLnetMocResourceGroup, s.vnetName)
	if err != nil || vnets == nil || len(*vnets) == 0 {
		s.logger.Error(err, "Failed to get VNet from MOC, skipping IPAM", "vnetName", s.vnetName)
		return false, nil
	}

	vnet := (*vnets)[0]

	// Check VNet is ArcVM-owned via known ownership tags
	if !isArcVMOwnedVNet(vnet.Tags) {
		s.logger.Info("VNet is not ArcVM-owned, skipping IPAM", "vnetName", s.vnetName)
		return false, nil
	}

	// Check VNet has subnets
	if vnet.VirtualNetworkPropertiesFormat == nil ||
		vnet.VirtualNetworkPropertiesFormat.Subnets == nil ||
		len(*vnet.VirtualNetworkPropertiesFormat.Subnets) == 0 {
		s.logger.Info("VNet has no subnets, skipping IPAM", "vnetName", s.vnetName)
		return false, nil
	}

	// Check subnet uses static IP allocation
	firstSubnet := (*vnet.VirtualNetworkPropertiesFormat.Subnets)[0]
	if firstSubnet.IPAllocationMethod != network.Static {
		s.logger.Info("VNet subnet not configured for Static IP allocation, skipping IPAM",
			"vnetName", s.vnetName, "allocationMethod", firstSubnet.IPAllocationMethod)
		return false, nil
	}

	s.logger.Info("VNet configured for Static IP allocation, proceeding with IPAM", "vnetName", s.vnetName)
	return true, nil
}

// AllocateIP creates an IPAddressClaim and waits for the IPAM operator to allocate an IP address.
// It first checks whether IPAM allocation is enabled for the configured VNet. If not, it returns
// ("", nil) without error. The optional additionalAnnotations maps are merged into the claim's
// annotations, allowing callers to attach MOC resource metadata (group, resource name, type).
// Returns the allocated IP address on success.
func (s *IPAMService) AllocateIP(ctx context.Context, claimName string, staticIP string, additionalAnnotations ...map[string]string) (allocatedIP string, err error) {
	logger := s.logger.WithValues("operation", "AllocateIP", "claimName", claimName)

	enableIPAMAllocation, err := s.isIPAMAllocationEnabled(ctx)
	if err != nil {
		return "", err
	}

	if !enableIPAMAllocation {
		logger.Info("IPAM not enabled for VNet, skipping allocation")
		return "", nil
	}

	params := s.buildIPClaimParams(claimName, staticIP, AllocationSourceOperatorIPAM, additionalAnnotations...)

	// Clean up the IPClaim on any error so the next reconcile starts fresh
	defer func() {
		if err != nil {
			if delErr := s.DeleteIPClaim(ctx, claimName); delErr != nil {
				logger.Error(delErr, "Failed to delete IPClaim after allocation failure")
			}
		}
	}()

	if err = s.createIPClaim(ctx, params); err != nil {
		s.telemetryWriter.WriteIPAMOperationLog(logger, OperationCreate, claimName,
			map[string]string{"requestedIP": staticIP}, err)
		return "", fmt.Errorf("failed to create IPClaim: %w", err)
	}

	allocatedIP, err = s.waitForIPAllocation(ctx, claimName)
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

// DeleteIPClaim deletes an IPAddressClaim by name and waits for the deletion to fully complete.
// If IPAM allocation is not enabled for the configured VNet, this is a no-op.
// Returns nil if the claim was successfully deleted or did not exist.
func (s *IPAMService) DeleteIPClaim(ctx context.Context, claimName string) (err error) {
	logger := s.logger.WithValues("operation", "DeleteIPClaim", "claimName", claimName)

	enableIPAMAllocation, err := s.isIPAMAllocationEnabled(ctx)
	if err != nil {
		return err
	}

	if !enableIPAMAllocation {
		logger.Info("IPAM not enabled for VNet, skipping IPAM operation")
		return nil
	}

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

// ensureIPClaimDeleted polls until the specified IPAddressClaim no longer exists in the cluster.
// It returns an error if the claim is not deleted within IPClaimTimeout.
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

// SyncIPClaim creates or re-creates an IPAddressClaim to track an IP that was already allocated
// externally (e.g., by MOC IPAM). This is best-effort and non-blocking — it does not wait for
// the IPAM operator to reconcile the claim. If an existing claim has a mismatched IP, it is
// deleted and recreated with the correct IP. The claim is annotated with AllocationSourceMOCIPAM
// to distinguish it from operator-allocated IPs. The optional additionalAnnotations maps are
// merged into the claim's annotations.
func (s *IPAMService) SyncIPClaim(ctx context.Context, claimName, allocatedIP string, additionalAnnotations ...map[string]string) error {
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

	// Only check with MOC if necessary 
	enableIPAMAllocation, enabledErr := s.isIPAMAllocationEnabled(ctx)
	if enabledErr != nil {
		return enabledErr
	}

	if !enableIPAMAllocation {
		return nil
	}

	// Just create, not waiting for completion
	// Note: If an IPClaim already existed with a mismatched IP, it was deleted above and
	// recreated here with AllocationSourceMOCIPAM, correctly reflecting that the final IP came from MOC.
	params := s.buildIPClaimParams(claimName, allocatedIP, AllocationSourceMOCIPAM, additionalAnnotations...)
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

// verifyAllocatedIP checks whether the IPAddress referenced by an IPAddressClaim matches the
// expected IP. It fetches the IPAddress object from the cluster and compares its Spec.Address.
// Returns nil if the IPs match, or an error describing the mismatch.
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
// The format is "ipclaim-{nicName}-{index}", where index identifies the IP position on
// multi-IP NICs.
func GenerateNICIPClaimName(nicName string, index int) string {
	return fmt.Sprintf("ipclaim-%s-%d", nicName, index)
}

// SetOwner updates the owner object used as the OwnerReference on newly created IPAddressClaims.
// This should be called when the owning resource changes (e.g., switching from cluster to machine).
func (s *IPAMService) SetOwner(owner client.Object) {
	s.owner = owner
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

// IsIPAMSoleAllocator determines whether the IPAM operator should be the sole IP allocator
// (i.e., MOC IPAM fallback is disabled). It checks for the presence of the azstackhci-operator
// deployment: if absent (azlocal-overlay extension scenario), IPAM is the sole allocator; if
// present, MOC IPAM fallback is preserved.
func (s *IPAMService) IsIPAMSoleAllocator(ctx context.Context) bool {
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{
		Name:      azstackhciOperatorDeploymentName,
		Namespace: azstackhciOperatorDeploymentNamespace,
	}

	err := s.client.Get(ctx, key, deployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// azstackhci-operator not deployed → azlocal-overlay extension → IPAM sole allocator
			return true
		}
		// API error → assume 2607 → keep MOC fallback
		return false
	}
	// azstackhci-operator deployed → 2607 → keep MOC fallback
	return false
}

// =============================================================================
// Internal helpers
// =============================================================================

// buildIPClaimParams assembles the parameters needed to create an IPAddressClaim, including
// base annotations (created-by, allocation-source) merged with any additionalAnnotations.
func (s *IPAMService) buildIPClaimParams(claimName, staticIP, allocationSource string, additionalAnnotations ...map[string]string) ipClaimParams {
	annotations := map[string]string{
		AnnotationIPClaimCreatedBy: s.creatorID,
	}
	if allocationSource != "" {
		annotations[AnnotationAllocationSource] = allocationSource
	}
	for _, extra := range additionalAnnotations {
		for k, v := range extra {
			annotations[k] = v
		}
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

// ipClaimParams holds the parameters for creating a single IPAddressClaim resource.
type ipClaimParams struct {
	Name        string
	Namespace   string
	ClusterName string
	VnetName    string
	StaticIP    string
	Annotations map[string]string
}

// createIPClaim creates an IPAddressClaim in the cluster from the given params. It sets
// annotations for VNet/subnet name and static IP (if provided), and attaches an OwnerReference
// to the service's owner object. If the claim already exists, it returns nil without error.
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

// waitForIPAllocation polls an IPAddressClaim until the IPAM operator populates its
// Status.AddressRef, then fetches and returns the allocated IP address. It checks for
// failure conditions (Ready=False) and returns a descriptive error on timeout.
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
