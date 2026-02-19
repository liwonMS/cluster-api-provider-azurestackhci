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

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/go-logr/logr"
	"github.com/microsoft/moc-sdk-for-go/services/network"
	"go.uber.org/multierr"

	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/scope"
	"github.com/microsoft/cluster-api-provider-azurestackhci/cloud/telemetry"
	ipam "github.com/microsoft/cluster-api-provider-azurestackhci/pkg/ipam"
)

// CAPHTelemetryWriter implements ipam.IPAMTelemetryWriter for CAPH.
type CAPHTelemetryWriter struct {
	vmScope *scope.VirtualMachineScope
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
		w.vmScope.GetCustomResourceTypeWithName(),
		telemetry.IPAddressClaim,
		telemetry.GenerateMocResourceName(w.vmScope.GetResourceGroup(), claimName),
		params,
		err,
	)

	logger.Info("IPAM operation Recorded")
}

// IPAMService wraps ipam.IPAMService for CAPH-specific functionality.
type IPAMService struct {
	*ipam.IPAMService
}

// NewIPAMService creates a new IPAM service instance.
func NewIPAMService(vmscope *scope.VirtualMachineScope) *IPAMService {
	logger := vmscope.GetLogger()

	config := ipam.IPAMServiceConfig{
		Client:          vmscope.Client(),
		Logger:          logger,
		Namespace:       vmscope.Namespace(),
		VnetName:        vmscope.VnetName(),
		CloudFqdn:       vmscope.CloudAgentFqdn,
		Authorizer:      vmscope.Authorizer,
		TelemetryWriter: &CAPHTelemetryWriter{vmScope: vmscope},
		ClusterName:     vmscope.ClusterName(),
		CreatorID:       ipam.IPClaimCreatorCAPH,
		Owner:           vmscope.AzureStackHCIVirtualMachine,
	}

	return &IPAMService{
		IPAMService: ipam.NewIPAMService(config),
	}
}

// AllocateNicIPClaim allocates IPClaims for all IP configurations on a NIC.
func (s *IPAMService) AllocateNicIPClaim(ctx context.Context, mocNic network.Interface, staticIPAddress string) error {
	var errs error
	for index := range *mocNic.IPConfigurations {
		claimName := ipam.GenerateNICIPClaimName(*mocNic.Name, index)
		if allocatedIP, err := s.AllocateIP(ctx, claimName, staticIPAddress, false); err != nil {
			errs = multierr.Append(errs, err)
		} else {
			(*mocNic.IPConfigurations)[index].InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress = to.StringPtr(allocatedIP)
		}
	}
	return errs
}

// SyncNicIPClaim syncs IPClaims for all IP configurations on a NIC.
func (s *IPAMService) SyncNicIPClaim(ctx context.Context, mocNic network.Interface) error {
	var errs error
	for index := range *mocNic.IPConfigurations {
		claimName := ipam.GenerateNICIPClaimName(*mocNic.Name, index)
		ipconfig := (*mocNic.IPConfigurations)[index]
		if ipconfig.InterfaceIPConfigurationPropertiesFormat != nil && ipconfig.InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress != nil {
			if err := s.SyncIPClaim(ctx, claimName, *(ipconfig.InterfaceIPConfigurationPropertiesFormat.PrivateIPAddress), false); err != nil {
				errs = multierr.Append(errs, err)
			}
		}
	}
	return errs
}

// DeleteNicIPClaim deletes IPClaims for all IP configurations on a NIC.
func (s *IPAMService) DeleteNicIPClaim(ctx context.Context, nicSpec *Spec) error {
	var errs error
	if len(nicSpec.IPConfigurations) == 0 {
		claimName := ipam.GenerateNICIPClaimName(nicSpec.Name, 0)
		if errs = s.DeleteIPClaim(ctx, claimName); errs != nil {
			return errs
		}
		return nil
	}

	for index := range nicSpec.IPConfigurations {
		claimName := ipam.GenerateNICIPClaimName(nicSpec.Name, index)
		if err := s.DeleteIPClaim(ctx, claimName); err != nil {
			errs = multierr.Append(errs, err)
		}
	}
	return errs
}
