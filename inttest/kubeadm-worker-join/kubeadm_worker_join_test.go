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

// Package kubeadmworkerjoin checks that a node provisioned with kubeadm can
// join a k0s cluster with `kubeadm join`, without any support for it in k0s.
//
// Like the capi-* tests, it runs against an existing kind management cluster
// (KUBECONFIG) that has Cluster API and k0smotron installed. The k0s cluster is
// a K0smotronControlPlane, hosted in the management cluster and exposed with a
// NodePort service, with no workers.
//
// The node joins with a regular k0s worker join token, created with
// `k0s token create` as for any k0s worker. The token is a kubeconfig with the
// cluster CA and a bootstrap token, which is what kubeadm's file discovery
// expects. k0s keeps anonymous authentication disabled, so kubeadm's token
// discovery, which reads the cluster-info anonymously, isn't an option.
// kubeadm reads the cluster-info with the bootstrap token, gets a 403 since
// there is none to read, and goes on with the file.
//
// k0s already lets bootstrap tokens request kubelet certificates and approves
// them. What kubeadm reads besides is provisioned through the
// K0smotronControlPlane only, as manifests that k0s applies from its manifests
// directory: kubeadm-config, kubelet-config and the RBAC for them.
//
// The kubeadm node is a kubeadm-node-join container, started directly with
// docker for the same reasons as in the capi-controlplane-kubeadm-remote-machine
// test. It joins through the NodePort of the control plane, on the "kind"
// docker network that the management cluster is on as well.
package kubeadmworkerjoin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/k0sproject/k0smotron/v2/inttest/util"

	"github.com/stretchr/testify/suite"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	clusterName   = "kubeadm-join-test"
	nodeContainer = "kubeadm-join-test-node"
	nodeName      = "kubeadm-worker"
	nodeImage     = "kubeadm-node-join"
	dockerNetwork = "kind"

	// The default API port of k0smotron control planes, as their NodePort.
	apiPort = 30443

	// The k0s version of the control plane, can be overridden with
	// K0S_CP_VERSION. The kubelet of the kubeadm node must not be newer than
	// its Kubernetes version, nor more than three minor versions older.
	defaultK0sVersion = "v1.33.3+k0s.0"

	// The group of all bootstrap tokens, which k0s worker tokens belong to.
	bootstrappersGroup = "system:bootstrappers"

	// Same kubelet fixups that Cluster API's docker provider applies to
	// kindest/node containers, see the kubeadm remote machine test.
	joinConfiguration = `
apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  file:
    kubeConfigPath: /tmp/discovery.conf
nodeRegistration:
  name: %s
  criSocket: unix:///var/run/containerd/containerd.sock
  kubeletExtraArgs:
  - name: fail-swap-on
    value: "false"
  - name: image-gc-high-threshold
    value: "100"
  - name: eviction-hard
    value: "nodefs.available<0%%,nodefs.inodesFree<0%%,imagefs.available<0%%"
  - name: cgroup-root
    value: /kubelet
  - name: runtime-cgroups
    value: /system.slice/containerd.service
  ignorePreflightErrors:
  - SystemVerification
  - Swap
  - FileContent--proc-sys-net-bridge-bridge-nf-call-iptables
`

	// What kubeadm reads while joining, as manifests for k0s to apply.
	joinManifests = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubeadm-config
  namespace: kube-system
data:
  ClusterConfiguration: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    clusterName: ` + clusterName + `
    kubernetesVersion: {{ .KubernetesVersion }}
    networking:
      dnsDomain: cluster.local
      serviceSubnet: 10.128.0.0/12
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubelet-config
  namespace: kube-system
data:
  kubelet: |
    apiVersion: kubelet.config.k8s.io/v1beta1
    kind: KubeletConfiguration
    cgroupDriver: systemd
    clusterDomain: cluster.local
    clusterDNS:
    - 10.128.0.10
---
# What joining nodes read with their bootstrap token.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubeadm:nodes-kubeadm-config
  namespace: kube-system
rules:
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [kubeadm-config]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kubeadm:nodes-kubeadm-config
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:nodes-kubeadm-config
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: ` + bootstrappersGroup + `
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:nodes
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubeadm:kubelet-config
  namespace: kube-system
rules:
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [kubelet-config]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kubeadm:kubelet-config
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:kubelet-config
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: ` + bootstrappersGroup + `
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:nodes
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeadm:get-nodes
rules:
- apiGroups: [""]
  resources: [nodes]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:get-nodes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeadm:get-nodes
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: ` + bootstrappersGroup + `
`
)

var clusterYaml = template.Must(template.New("cluster").Funcs(template.FuncMap{
	// indent indents all lines but the first, which is placed by the template.
	"indent": func(n int, s string) string {
		return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+strings.Repeat(" ", n))
	},
}).Parse(`
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: ` + clusterName + `
  namespace: default
spec:
  clusterNetwork:
    pods:
      cidrBlocks:
      - 192.168.0.0/16
    serviceDomain: cluster.local
    services:
      cidrBlocks:
      - 10.128.0.0/12
  controlPlaneRef:
    apiVersion: controlplane.cluster.x-k8s.io/v1beta1
    kind: K0smotronControlPlane
    name: ` + clusterName + `
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
    kind: DevCluster
    name: ` + clusterName + `
---
apiVersion: v1
kind: Secret
metadata:
  name: ` + clusterName + `-join-manifests
  namespace: default
stringData:
  join.yaml: |
    {{ indent 4 .JoinManifests }}
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: K0smotronControlPlane
metadata:
  name: ` + clusterName + `
  namespace: default
spec:
  version: {{ .K0sVersion }}
  service:
    type: NodePort
  manifests:
  - name: kubeadm-join
    secret:
      secretName: ` + clusterName + `-join-manifests
  k0sConfig:
    apiVersion: k0s.k0sproject.io/v1beta1
    kind: ClusterConfig
    spec:
      telemetry:
        enabled: false
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: DevCluster
metadata:
  name: ` + clusterName + `
  namespace: default
spec:
  backend:
    docker: {}
`))

type KubeadmWorkerJoinSuite struct {
	suite.Suite

	ctx              context.Context
	client           *kubernetes.Clientset
	restConfig       *rest.Config
	clusterYamlsPath string
}

func TestKubeadmWorkerJoinSuite(t *testing.T) {
	suite.Run(t, &KubeadmWorkerJoinSuite{})
}

func (s *KubeadmWorkerJoinSuite) SetupSuite() {
	kubeConfigPath := os.Getenv("KUBECONFIG")
	s.Require().NotEmpty(kubeConfigPath, "KUBECONFIG env var must be set and point to kind cluster")

	k0sVersion := os.Getenv("K0S_CP_VERSION")
	if k0sVersion == "" {
		k0sVersion = defaultK0sVersion
	}
	kubernetesVersion, _, _ := strings.Cut(k0sVersion, "+")

	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	s.Require().NoError(err)
	s.restConfig = restCfg
	s.client, err = kubernetes.NewForConfig(restCfg)
	s.Require().NoError(err)

	// The manifests are templates themselves, for the Kubernetes version.
	manifests, err := template.New("manifests").Parse(joinManifests)
	s.Require().NoError(err)
	var renderedManifests bytes.Buffer
	s.Require().NoError(manifests.Execute(&renderedManifests, struct{ KubernetesVersion string }{kubernetesVersion}))

	var yaml bytes.Buffer
	s.Require().NoError(clusterYaml.Execute(&yaml, struct{ K0sVersion, JoinManifests string }{
		K0sVersion:    k0sVersion,
		JoinManifests: renderedManifests.String(),
	}))
	s.clusterYamlsPath = s.T().TempDir() + "/cluster.yaml"
	s.Require().NoError(os.WriteFile(s.clusterYamlsPath, yaml.Bytes(), 0644))

	s.ctx, _ = util.NewSuiteContext(s.T())
}

func (s *KubeadmWorkerJoinSuite) TestKubeadmWorkerJoinsK0smotronControlPlane() {
	ctx := s.ctx

	s.T().Log("creating the k0s control plane, provisioned with what kubeadm needs")
	out, err := exec.Command("kubectl", "apply", "-f", s.clusterYamlsPath).CombinedOutput()
	s.Require().NoError(err, "failed to apply cluster objects: %s", out)
	defer func() {
		keep := os.Getenv("KEEP_AFTER_TEST")
		if keep == "true" || (keep == "on-failure" && s.T().Failed()) {
			return
		}
		s.T().Log("deleting cluster objects")
		s.Require().NoError(util.DeleteCluster(clusterName))
		_ = exec.Command("kubectl", "delete", "--ignore-not-found", "-f", s.clusterYamlsPath).Run()
		s.T().Log("removing the node container")
		_ = exec.Command("docker", "rm", "-f", nodeContainer).Run()
	}()

	s.T().Log("waiting for the control plane pod")
	s.Require().NoError(util.WaitForStatefulSet(ctx, s.client, "kmc-"+clusterName, "default"))

	fw, err := util.GetPortForwarder(s.restConfig, "kmc-"+clusterName+"-0", "default", apiPort)
	s.Require().NoError(err)
	go fw.Start(s.Require().NoError)
	defer fw.Close()
	<-fw.ReadyChan
	localPort, err := fw.LocalPort()
	s.Require().NoError(err)

	s.T().Log("waiting for the k0s API")
	kc, err := kubernetes.NewForConfig(s.waitForK0sKubeconfig(ctx, localPort))
	s.Require().NoError(err)

	s.T().Log("waiting for k0s to apply the manifests for kubeadm")
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := kc.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, "kubeadm-config", metav1.GetOptions{})
		if err != nil {
			s.T().Logf("kubeadm-config not there yet: %v", err)
			return false, nil
		}
		return true, nil
	}), "k0s should apply the manifests for kubeadm")

	s.T().Log("creating a k0s worker join token")
	joinToken, err := util.GetJoinToken(s.client, s.restConfig, "kmc-"+clusterName+"-0", "default")
	s.Require().NoError(err)
	discoveryConfig, err := decodeJoinToken(joinToken)
	s.Require().NoError(err)
	apiServer, tokenID, err := joinTokenServerAndID(discoveryConfig)
	s.Require().NoError(err)

	s.T().Log("starting the kubeadm node container")
	s.startNodeContainer(ctx)
	s.prepareNode(ctx)

	s.T().Log("checking that the API server rejects anonymous requests")
	s.Equal(fmt.Sprint(http.StatusUnauthorized), s.anonymousStatus(ctx, apiServer, "/api/v1/namespaces/kube-public/configmaps/cluster-info"))

	s.dockerExec(ctx, string(discoveryConfig), "sh", "-c", "cat > /tmp/discovery.conf")

	s.T().Logf("running kubeadm join against %s", apiServer)
	joinConfig := fmt.Sprintf(joinConfiguration, nodeName)
	s.dockerExec(ctx, joinConfig, "sh", "-c", "cat > /tmp/join.yaml")
	s.T().Logf("kubeadm join output:\n%s", s.dockerExec(ctx, "", "kubeadm", "join", "--config=/tmp/join.yaml", "--v=5"))

	s.T().Log("waiting for the node to register")
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := kc.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		return err == nil, nil
	}), "the kubeadm node should register at the k0s cluster")

	s.T().Log("checking that the kubelet's certificate request got approved")
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		return hasIssuedKubeletCSR(ctx, kc, tokenID), nil
	}), "the CSR of the kubeadm node should be approved and issued")

	// The node can't be expected to be Ready without a CNI that suits it, so
	// only report it.
	node, err := kc.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	s.Require().NoError(err)
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			s.T().Logf("node %s Ready=%s (%s)", nodeName, cond.Status, cond.Reason)
		}
	}
}

// waitForK0sKubeconfig waits for the admin kubeconfig that k0smotron creates
// for the k0s cluster and returns its client configuration, pointing to the
// local port forwarded to the control plane pod as the capi-* tests do.
func (s *KubeadmWorkerJoinSuite) waitForK0sKubeconfig(ctx context.Context, localPort int) *rest.Config {
	secretName := clusterName + "-kubeconfig"
	s.Require().NoError(util.WaitForSecret(ctx, s.client, secretName, "default"))
	secret, err := s.client.CoreV1().Secrets("default").Get(ctx, secretName, metav1.GetOptions{})
	s.Require().NoError(err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	s.Require().NoError(err)
	cfg.Host = fmt.Sprintf("localhost:%d", localPort)
	return cfg
}

// decodeJoinToken decodes a k0s join token, a gzipped and base64 encoded
// kubeconfig with the cluster CA and a bootstrap token.
func decodeJoinToken(token string) ([]byte, error) {
	gzData, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return nil, fmt.Errorf("failed to decode the join token: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress the join token: %w", err)
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

// joinTokenServerAndID returns the API server address and the ID of the
// bootstrap token of a decoded k0s join token.
func joinTokenServerAndID(kubeconfig []byte) (string, string, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse the join token: %w", err)
	}
	kubeContext, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return "", "", fmt.Errorf("no current context in the join token")
	}
	cluster, ok := cfg.Clusters[kubeContext.Cluster]
	if !ok {
		return "", "", fmt.Errorf("no cluster %q in the join token", kubeContext.Cluster)
	}
	authInfo, ok := cfg.AuthInfos[kubeContext.AuthInfo]
	if !ok {
		return "", "", fmt.Errorf("no user %q in the join token", kubeContext.AuthInfo)
	}
	tokenID, _, found := strings.Cut(authInfo.Token, ".")
	if !found {
		return "", "", fmt.Errorf("no bootstrap token in the join token")
	}
	return cluster.Server, tokenID, nil
}

// startNodeContainer runs the kubeadm-node-join image with the privileged,
// security and cgroup settings that kindest/node needs to boot systemd,
// containerd and the kubelet.
func (s *KubeadmWorkerJoinSuite) startNodeContainer(ctx context.Context) {
	_ = exec.Command("docker", "rm", "-f", nodeContainer).Run()

	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", nodeContainer,
		"--hostname", nodeName,
		"--network", dockerNetwork,
		"--privileged",
		"--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined",
		"--cgroupns=private",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"-v", "/lib/modules:/lib/modules:ro",
		"-v", "/var",
		nodeImage,
	).CombinedOutput()
	s.Require().NoError(err, "failed to start the node container: %s", out)
}

// prepareNode waits for containerd and applies the kernel settings that
// kubeadm's preflight checks and kube-proxy expect, best effort as in CAPD.
func (s *KubeadmWorkerJoinSuite) prepareNode(ctx context.Context) {
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		err := exec.CommandContext(ctx, "docker", "exec", nodeContainer, "systemctl", "is-active", "--quiet", "containerd").Run()
		return err == nil, nil
	}), "containerd should be running in the node container")

	s.dockerExec(ctx, "", "sh", "-c", strings.Join([]string{
		"modprobe overlay || true",
		"modprobe br_netfilter || true",
		"sysctl -w net.bridge.bridge-nf-call-iptables=1 || true",
		"sysctl -w net.bridge.bridge-nf-call-ip6tables=1 || true",
		"sysctl -w net.ipv4.ip_forward=1 || true",
	}, "; "))
}

// dockerExec runs the command in the node container, feeding it stdin if not
// empty, and returns the combined output. It fails the test on errors.
func (s *KubeadmWorkerJoinSuite) dockerExec(ctx context.Context, stdin string, command ...string) string {
	args := []string{"exec"}
	if stdin != "" {
		args = append(args, "-i")
	}
	args = append(args, nodeContainer)
	args = append(args, command...)

	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	s.Require().NoError(cmd.Run(), "%v failed:\n%s", command, out.String())
	return out.String()
}

// anonymousStatus requests the URL from the node container without
// credentials, and returns the HTTP status code.
func (s *KubeadmWorkerJoinSuite) anonymousStatus(ctx context.Context, apiServer, path string) string {
	out, err := exec.CommandContext(ctx, "docker", "exec", nodeContainer,
		"curl", "-sk", "-o", "/dev/null", "-w", "%{http_code}", apiServer+path).Output()
	s.Require().NoError(err)
	return strings.TrimSpace(string(out))
}

// hasIssuedKubeletCSR reports whether a kubelet client CSR requested by the
// bootstrap token has been approved and signed.
func hasIssuedKubeletCSR(ctx context.Context, kc kubernetes.Interface, tokenID string) bool {
	csrs, err := kc.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false
	}
	for _, csr := range csrs.Items {
		if csr.Spec.Username != "system:bootstrap:"+tokenID || len(csr.Status.Certificate) == 0 {
			continue
		}
		for _, cond := range csr.Status.Conditions {
			if cond.Type == certificatesv1.CertificateApproved {
				return true
			}
		}
	}
	return false
}
