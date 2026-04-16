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
	"bytes"
	"fmt"
	"os/exec"
	"testing"

	"github.com/k0sproject/k0s/inttest/common"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	podexec "github.com/k0sproject/k0smotron/internal/exec"
	corev1 "k8s.io/api/core/v1"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
)

type IngressValidator struct{}

func (v *IngressValidator) PreClusterInitialization() (map[string]string, error) {
	// Install HAProxy ingress controller
	if err := installHAProxyIngress(bootstrapClusterProxy); err != nil {
		return nil, fmt.Errorf("failed to install HAProxy ingress controller: %w", err)
	}

	kindIP, err := detectKindIP()
	if err != nil {
		return nil, fmt.Errorf("failed to detect kind IP: %w", err)
	}

	return map[string]string{
		"KIND_IP":      kindIP,
		"HAPROXY_PORT": "32143", // HAProxy svc NodePort for HTTPS
	}, nil
}

func (v *IngressValidator) Validate(t *testing.T, cluster *clusterv1.Cluster) {
	t.Log("Check kube api connection from the nodes through the proxy")
	machineList := &clusterv1.MachineList{}
	require.NoError(t, bootstrapClusterProxy.GetClient().List(ctx, machineList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel: cluster.Name,
	}))

	workloadCluster := bootstrapClusterProxy.GetWorkloadCluster(ctx, cluster.Namespace, cluster.Name, capiframework.WithRESTConfigModifier(func(config *rest.Config) {
		config.Host = "https://localhost:30443"
	}))
	wcs, err := kubernetes.NewForConfig(workloadCluster.GetRESTConfig())
	require.NoError(t, err, "Should get workload clientset")
	require.NoError(t, common.WaitForDaemonSet(ctx, wcs, "konnectivity-agent"))

	podList := &corev1.PodList{}
	require.NoError(t, bootstrapClusterProxy.GetClient().List(ctx, podList, client.InNamespace(cluster.Namespace)))
	out, err := podexec.PodExecCmdOutput(ctx, bootstrapClusterProxy.GetClientSet(), bootstrapClusterProxy.GetRESTConfig(), podList.Items[0].Name, cluster.Namespace, "k0s kc logs -n kube-system ds/konnectivity-agent")
	require.NoError(t, err, "Failed to get konnectivity agent logs")
	t.Logf("Konnectivity agent logs:\n%s", out)
	require.Contains(t, out, "change detected in proxy", "Expected log message not found in konnectivity agent logs")

	for _, m := range machineList.Items {
		var (
			stdout bytes.Buffer
			stderr bytes.Buffer
		)
		cmd := exec.Command("docker", "exec", m.Name, "curl", "https://10.128.0.1/healthz", "--cacert", "/etc/haproxy/certs/ca.crt")
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err = cmd.Run()
		require.NoError(t, err, stderr.String())
		require.Equal(t, "ok", stdout.String(), "Expected to get 'ok' from the healthz endpoint")
	}
}

func (v *IngressValidator) PostClusterDeletion(_ *testing.T, _ *types.NamespacedName) error {
	// No cleanup needed for HAProxy as it is installed in the bootstrap cluster and will be cleaned up with it
	return nil
}

func detectKindIP() (string, error) {
	// Get the kind cluster IP from the bootstrap cluster
	var nodes corev1.NodeList
	err := bootstrapClusterProxy.GetClient().List(ctx, &nodes)
	if err != nil {
		return "", fmt.Errorf("failed to list nodes: %w", err)
	}

	for _, node := range nodes.Items {
		if node.Spec.ProviderID != "" && len(node.Status.Addresses) > 0 {
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					// Extract IP from provider ID or use the address directly
					ip := addr.Address
					if ip != "" && ip != "127.0.0.1" {
						return ip, nil
					}
				}
			}
		}
	}

	return "", fmt.Errorf("failed to detect kind IP")
}

func installHAProxyIngress(bootstrapClusterProxy capiframework.ClusterProxy) error {
	out, err := exec.Command("kubectl", "--kubeconfig", bootstrapClusterProxy.GetKubeconfigPath(), "apply", "-f", "./data/haproxy-ingress.yaml").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to install HAProxy ingress controller: %w, output: %s", err, string(out))
	}
	return nil
}
