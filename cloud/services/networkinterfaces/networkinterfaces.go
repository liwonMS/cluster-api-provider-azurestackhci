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

package networkinterfaces

import (
	"context"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	azurestackhci "github.com/microsoft/cluster-api-provider-azurestackhci/cloud"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/telemetry"
	"github.com/microsoft/moc-sdk-for-go/services/network"
	mocerrors "github.com/microsoft/moc/pkg/errors"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Spec specification for ip configuration
type IPConfiguration struct {
	Name    string
	Primary bool
}

type IPConfigurations []*IPConfiguration

// Spec specification for network interface
type Spec struct {
	Name             string
	SubnetName       string
	VnetName         string
	StaticIPAddress  string
	MacAddress       string
	BackendPoolNames []string
	IPConfigurations IPConfigurations
	IPAMService      *IPAMService
}

// Get provides information about a network interface.
func (s *Service) Get(ctx context.Context, spec interface{}) (interface{}, error) {
	nicSpec, ok := spec.(*Spec)
	if !ok {
		return network.Interface{}, errors.New("invalid network interface specification")
	}
	nic, err := s.Client.Get(ctx, s.Scope.GetResourceGroup(), nicSpec.Name)
	if err != nil {
		return nil, err
	}
	return (*nic)[0], nil
}

// Reconcile gets/creates/updates a network interface.
func (s *Service) Reconcile(ctx context.Context, spec interface{}) error {
	telemetry.WriteMocInfoLog(ctx, s.Scope)
	nicSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("invalid network interface specification")
	}

	if nic, err := s.Get(ctx, nicSpec); err == nil {
		// Nic already exists, no update supported for now
		// Sync back to IPAM to ensure claim exists
		s.Scope.GetLogger().Info("Nic exists, attempting to sync IPClaim", "name", nicSpec.Name)
		mocNic := nic.(network.Interface)
		if err := s.SyncNicIPClaim(ctx, mocNic); err != nil {
			s.Scope.GetLogger().Info("Failed to sync IPClaim during reconcile", "error", err)
			// Non-blocking - don't fail NIC reconcile
		}
		return nil
	}

	logger := s.Scope.GetLogger()
	nicConfig := &network.InterfaceIPConfigurationPropertiesFormat{}
	nicConfig.Subnet = &network.APIEntityReference{
		ID: to.StringPtr(nicSpec.VnetName),
	}
	backendAddressPools := []network.BackendAddressPool{}
	for _, backendpoolname := range nicSpec.BackendPoolNames {
		name := backendpoolname
		backendAddressPools = append(backendAddressPools, network.BackendAddressPool{Name: &name})
	}
	nicConfig.LoadBalancerBackendAddressPools = &backendAddressPools

	if nicSpec.StaticIPAddress != "" {
		nicConfig.PrivateIPAddress = to.StringPtr(nicSpec.StaticIPAddress)
	}

	networkInterface := network.Interface{
		Name: &nicSpec.Name,
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			EnableIPForwarding: to.BoolPtr(true),
			EnableMACSpoofing:  to.BoolPtr(true),
			MacAddress:         &nicSpec.MacAddress,
			IPConfigurations:   &[]network.InterfaceIPConfiguration{},
		},
	}

	if len(nicSpec.IPConfigurations) > 0 {
		logger.Info("Adding ipconfigurations to nic ", "len", len(nicSpec.IPConfigurations), "name", nicSpec.Name)
		for _, ipconfig := range nicSpec.IPConfigurations {
			networkIPConfig := network.InterfaceIPConfiguration{
				Name: &ipconfig.Name,
				InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
					Primary: &ipconfig.Primary,
					Subnet: &network.APIEntityReference{
						ID: to.StringPtr(nicSpec.VnetName),
					},
				},
			}

			if ipconfig.Primary {
				networkIPConfig.LoadBalancerBackendAddressPools = &backendAddressPools
			}

			*networkInterface.IPConfigurations = append(*networkInterface.IPConfigurations, networkIPConfig)
		}
	} else {
		networkIPConfig := network.InterfaceIPConfiguration{
			Name:                                     to.StringPtr("pipConfig"),
			InterfaceIPConfigurationPropertiesFormat: nicConfig,
		}

		*networkInterface.IPConfigurations = append(*networkInterface.IPConfigurations, networkIPConfig)
	}

	// assign ipam IP to the moc nic object.
	if err := s.AllocateNicIPClaim(ctx, networkInterface, nicSpec.StaticIPAddress); err != nil {
		logger.Error(err, "Failed to allocate IPClaim for network interface", "name", nicSpec.Name)
	}

	logger.Info("creating network interface ", "name", nicSpec.Name)

	_, err := s.Client.CreateOrUpdate(ctx,
		s.Scope.GetResourceGroup(),
		nicSpec.Name,
		&networkInterface)
	telemetry.WriteMocOperationLog(s.Scope.GetLogger(), telemetry.CreateOrUpdate, s.Scope.GetCustomResourceTypeWithName(), telemetry.NetworkInterface,
		telemetry.GenerateMocResourceName(s.Scope.GetResourceGroup(), nicSpec.Name), &networkInterface, err)
	if err != nil {
		if s.shouldRetryIfIPConflict(err, nicSpec) {
			if err := s.handleIPAddressConflictRetry(ctx, nicSpec, &networkInterface); err != nil {
				return err
			}

			logger.Info("successfully created network interface ", "name", nicSpec.Name)
			return nil
		}
		return errors.Wrapf(err, "failed to create network interface %s in resource group %s", nicSpec.Name, s.Scope.GetResourceGroup())
	}

	logger.Info("successfully created network interface ", "name", nicSpec.Name)
	return err
}

// isIPConflictError checks if the error indicates an IP address conflict that should trigger a retry
func (s *Service) shouldRetryIfIPConflict(err error, nicSpec *Spec) bool {
	// user specified static IP, no need to retry
	if err == nil || nicSpec.StaticIPAddress != "" {
		return false
	}

	// Check for the specific error pattern indicating IP address conflict
	return mocerrors.IsAlreadySet(err)
}

func (s *Service) handleIPAddressConflictRetry(ctx context.Context, vnicSpec *Spec, networkInterface *network.Interface) error {
	logger := s.Scope.GetLogger()
	logger.Info("IP allocated by IPAM is already taken in Moc, retrying", "IP", (*networkInterface.IPConfigurations)[0].InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress)

	// Remove the failed mocnetworkinterface
	if err := s.Delete(ctx, vnicSpec); err != nil {
		return err
	}

	for _, ipconfig := range *networkInterface.IPConfigurations {
		ipconfig.InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress = nil
	}

	// Recreate the mocnetworkinterface without the IPAM allocated IP
	_, err := s.Client.CreateOrUpdate(ctx,
		s.Scope.GetResourceGroup(),
		vnicSpec.Name,
		networkInterface)

	telemetry.WriteMocOperationLog(s.Scope.GetLogger(), telemetry.CreateOrUpdate, s.Scope.GetCustomResourceTypeWithName(), telemetry.NetworkInterface,
		telemetry.GenerateMocResourceName(s.Scope.GetResourceGroup(), vnicSpec.Name), &networkInterface, err)

	return err
}

// Delete deletes the network interface with the provided name.
func (s *Service) Delete(ctx context.Context, spec interface{}) error {
	telemetry.WriteMocInfoLog(ctx, s.Scope)
	nicSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("invalid network interface Specification")
	}
	logger := s.Scope.GetLogger()
	logger.Info("deleting nic", "name", nicSpec.Name)
	defer func() {
		if err := s.DeleteNicIPClaim(ctx, nicSpec); err != nil {
			logger.Error(err, "failed to delete IPAM claim for nic", nicSpec.Name)
		}
	}()

	err := s.Client.Delete(ctx, s.Scope.GetResourceGroup(), nicSpec.Name)
	telemetry.WriteMocOperationLog(logger, telemetry.Delete, s.Scope.GetCustomResourceTypeWithName(), telemetry.NetworkInterface,
		telemetry.GenerateMocResourceName(s.Scope.GetResourceGroup(), nicSpec.Name), nil, err)
	if err != nil && azurestackhci.ResourceNotFound(err) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "failed to delete network interface %s in resource group %s", nicSpec.Name, s.Scope.GetResourceGroup())
	}

	err = s.ensureNicDeleted(ctx, nicSpec)
	if err != nil {
		return errors.Wrapf(err, "timed out waiting for deletion of network interface %s in resource group %s", nicSpec.Name, s.Scope.GetResourceGroup())
	}

	logger.Info("successfully deleted nic", "name", nicSpec.Name)
	return err
}

// ensureNicDeleted ensures the network interface is deleted by polling Get with a 5 second timeout.
func (s *Service) ensureNicDeleted(ctx context.Context, nicSpec *Spec) error {
	logger := s.Scope.GetLogger()

	pollErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := s.Get(ctx, nicSpec)
		if err != nil {
			if azurestackhci.ResourceNotFound(err) {
				logger.Info("nic not found", "name", nicSpec.Name)
				return true, nil // Deletion complete
			}
			logger.Error(err, "failed to get nic", "name", nicSpec.Name)
			return false, err
		}
		logger.Info("nic still exists, waiting for deletion", "name", nicSpec.Name)
		return false, nil // Continue polling
	})

	if pollErr != nil {
		return errors.Wrapf(pollErr, "failed waiting for nic %s to be deleted", nicSpec.Name)
	}

	return nil
}

func (s *Service) AllocateNicIPClaim(ctx context.Context, mocNic network.Interface, staticIPAddress string) error {
	var errs error
	for index, ipconfig := range *mocNic.IPConfigurations {
		claimName := s.IPAMService.GenerateIPClaimName(*mocNic.Name, index)
		if allocatedIP, err := s.IPAMService.AllocateIPClaim(ctx, claimName, staticIPAddress); err != nil {
			s.Scope.GetLogger().Info("Failed to allocate IPClaim during reconcile", "error", err)
			errs = multierr.Append(errs, err)
		} else {
			ipconfig.InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress = to.StringPtr(allocatedIP)
		}
	}

	return errs
}

func (s *Service) SyncNicIPClaim(ctx context.Context, mocNic network.Interface) error {
	var errs error
	for index, ipconfig := range *mocNic.IPConfigurations {
		claimName := s.IPAMService.GenerateIPClaimName(*mocNic.Name, index)
		if err := s.IPAMService.SyncIPClaim(ctx, claimName, *(ipconfig.InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress)); err != nil {
			s.Scope.GetLogger().Info("Failed to sync IPClaim during reconcile", "error", err)
			errs = multierr.Append(errs, err)
		}
	}

	return errs
}

func (s *Service) DeleteNicIPClaim(ctx context.Context, nicSpec *Spec) error {
	var errs error
	for index := range nicSpec.IPConfigurations {
		claimName := s.IPAMService.GenerateIPClaimName(nicSpec.Name, index)
		if err := s.IPAMService.DeleteIPClaim(ctx, claimName); err != nil {
			s.Scope.GetLogger().Info("Failed to delete IPClaim during reconcile", "error", err)
			errs = multierr.Append(errs, err)
		}
	}

	return errs
}
