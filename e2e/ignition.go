//go:build e2e

/*
Copyright 2025.

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

package e2e

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

type IgnitionValidator struct{}

func (v *IgnitionValidator) PreClusterInitialization() (map[string]string, error) {
	// // A SSH is not really needed for using AWS, but for debugging purposes it is useful to have it configured.
	sshPublicKey := e2eConfig.MustGetVariable(SSHPublicKey)
	if sshPublicKey == "" {
		return nil, fmt.Errorf("SSH public key is not set")
	}
	sshKeyName := e2eConfig.MustGetVariable(SSHKeyName)
	if sshKeyName == "" {
		return nil, fmt.Errorf("SSH key name is not set")
	}

	return map[string]string{
		"SSH_PUBLIC_KEY": sshPublicKey,
		"SSH_KEY_NAME":   sshKeyName,
	}, nil

}
func (v *IgnitionValidator) Validate(_ *testing.T, _ *clusterv1.Cluster) {}
func (v *IgnitionValidator) PostClusterDeletion(_ *testing.T, _ *types.NamespacedName) error {
	return nil
}
