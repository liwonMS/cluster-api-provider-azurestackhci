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

// ConvertTo converts this v1beta1 AzureStackHCIMachine to the Hub version (v1beta2).
func (src *AzureStackHCIMachine) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*infrav1beta2.AzureStackHCIMachine)
	return Convert_v1beta1_AzureStackHCIMachine_To_v1beta2_AzureStackHCIMachine(src, dst, nil)
}

// ConvertFrom converts from the Hub version (v1beta2) to this v1beta1 AzureStackHCIMachine.
func (dst *AzureStackHCIMachine) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*infrav1beta2.AzureStackHCIMachine)
	return Convert_v1beta2_AzureStackHCIMachine_To_v1beta1_AzureStackHCIMachine(src, dst, nil)
}

// ConvertTo converts this v1beta1 AzureStackHCIMachineList to the Hub version (v1beta2).
func (src *AzureStackHCIMachineList) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*infrav1beta2.AzureStackHCIMachineList)
	return Convert_v1beta1_AzureStackHCIMachineList_To_v1beta2_AzureStackHCIMachineList(src, dst, nil)
}

// ConvertFrom converts from the Hub version (v1beta2) to this v1beta1 AzureStackHCIMachineList.
func (dst *AzureStackHCIMachineList) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*infrav1beta2.AzureStackHCIMachineList)
	return Convert_v1beta2_AzureStackHCIMachineList_To_v1beta1_AzureStackHCIMachineList(src, dst, nil)
}
