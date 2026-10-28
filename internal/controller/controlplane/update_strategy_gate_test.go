/*
Copyright 2026.

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

package controlplane

import (
	"testing"

	cpv1beta2 "github.com/k0sproject/k0smotron/v2/api/controlplane/v1beta2"
	"github.com/k0sproject/k0smotron/v2/internal/featuregate"
)

func TestInPlaceVersionWithDeleteFirstGate(t *testing.T) {
	tests := []struct {
		strategy        cpv1beta2.UpdateStrategy
		gate            bool
		wantInPlace     bool
		wantDeleteFirst bool
	}{
		{cpv1beta2.UpdateInPlace, false, true, false},
		{cpv1beta2.UpdateRecreateDeleteFirst, false, false, true},
		{cpv1beta2.UpdateRecreate, false, false, false},
		{cpv1beta2.UpdateInPlace, true, true, true},
		{cpv1beta2.UpdateRecreateDeleteFirst, true, true, true},
		{cpv1beta2.UpdateRecreate, true, false, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.strategy), func(t *testing.T) {
			if err := featuregate.Configure("InPlaceVersionWithDeleteFirst="+map[bool]string{true: "true", false: "false"}[tc.gate], ""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = featuregate.Configure("InPlaceVersionWithDeleteFirst=false", "") })

			kcp := &cpv1beta2.K0sControlPlane{Spec: cpv1beta2.K0sControlPlaneSpec{UpdateStrategy: tc.strategy}}
			if got := isVersionInPlaceEnabled(kcp); got != tc.wantInPlace {
				t.Errorf("isVersionInPlaceEnabled = %v, want %v", got, tc.wantInPlace)
			}
			if got := isDeleteFirstStrategyEnabled(kcp); got != tc.wantDeleteFirst {
				t.Errorf("isDeleteFirstStrategyEnabled = %v, want %v", got, tc.wantDeleteFirst)
			}
		})
	}
}
