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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsIPAMSupported(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	tests := []struct {
		name    string
		objects []client.Object
		want    bool
	}{
		{
			name: "arcappliance offer returns true",
			objects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cloudOpProductInfoConfigMapName,
						Namespace: cloudOpProductInfoConfigMapNamespace,
					},
					Data: map[string]string{
						productInfoOfferKey: offerAzureLocal,
					},
				},
			},
			want: true,
		},
		{
			name: "aks-hci-releases offer returns false (22H2)",
			objects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cloudOpProductInfoConfigMapName,
						Namespace: cloudOpProductInfoConfigMapNamespace,
					},
					Data: map[string]string{
						productInfoOfferKey: offer22H2,
					},
				},
			},
			want: false,
		},
		{
			name:    "configmap not found returns true (fail-open)",
			objects: []client.Object{},
			want:    true,
		},
		{
			name: "configmap exists but missing offer key returns true",
			objects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cloudOpProductInfoConfigMapName,
						Namespace: cloudOpProductInfoConfigMapNamespace,
					},
					Data: map[string]string{
						"someOtherKey": "someValue",
					},
				},
			},
			want: true,
		},
		{
			name: "configmap with empty offer returns true",
			objects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cloudOpProductInfoConfigMapName,
						Namespace: cloudOpProductInfoConfigMapNamespace,
					},
					Data: map[string]string{
						productInfoOfferKey: "",
					},
				},
			},
			want: true,
		},
		{
			name: "unknown offer value returns true",
			objects: []client.Object{
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cloudOpProductInfoConfigMapName,
						Namespace: cloudOpProductInfoConfigMapNamespace,
					},
					Data: map[string]string{
						productInfoOfferKey: "some-future-offer",
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.objects...).
				Build()

			got := IsIPAMSupported(context.Background(), fakeClient)
			if got != tt.want {
				t.Errorf("IsIPAMSupported() = %v, want %v", got, tt.want)
			}
		})
	}
}
