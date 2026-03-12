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

package v1beta1

import (
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	infrav1beta2 "github.com/microsoft/cluster-api-provider-azurestackhci/api/v1beta2"
)

// ConvertTo converts this v1beta1 AzureStackHCIVirtualMachine to the Hub version (v1beta2).
func (src *AzureStackHCIVirtualMachine) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*infrav1beta2.AzureStackHCIVirtualMachine)
	return Convert_v1beta1_AzureStackHCIVirtualMachine_To_v1beta2_AzureStackHCIVirtualMachine(src, dst, nil)
}

// ConvertFrom converts from the Hub version (v1beta2) to this v1beta1 AzureStackHCIVirtualMachine.
func (dst *AzureStackHCIVirtualMachine) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*infrav1beta2.AzureStackHCIVirtualMachine)
	return Convert_v1beta2_AzureStackHCIVirtualMachine_To_v1beta1_AzureStackHCIVirtualMachine(src, dst, nil)
}

// ConvertTo converts this v1beta1 AzureStackHCIVirtualMachineList to the Hub version (v1beta2).
func (src *AzureStackHCIVirtualMachineList) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*infrav1beta2.AzureStackHCIVirtualMachineList)
	return Convert_v1beta1_AzureStackHCIVirtualMachineList_To_v1beta2_AzureStackHCIVirtualMachineList(src, dst, nil)
}

// ConvertFrom converts from the Hub version (v1beta2) to this v1beta1 AzureStackHCIVirtualMachineList.
func (dst *AzureStackHCIVirtualMachineList) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*infrav1beta2.AzureStackHCIVirtualMachineList)
	return Convert_v1beta2_AzureStackHCIVirtualMachineList_To_v1beta1_AzureStackHCIVirtualMachineList(src, dst, nil)
}
