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
// a K0sControlPlane on the docker infrastructure provider, with a single
// controller and no workers.
//
// Everything that kubeadm expects to find in the cluster is provisioned through
// the K0sControlPlane only, as manifests that k0s applies from its manifests
// directory: the bootstrap token, kubeadm-config, kubelet-config and the RBAC
// for them.
//
// k0s keeps anonymous authentication disabled, so the node doesn't use token
// discovery, which reads the cluster-info anonymously. It uses file discovery
// instead, with a kubeconfig that carries the cluster CA and the bootstrap
// token. kubeadm reads the cluster-info with that token, gets a 403 since
// there is none to read, and goes on with the file.
//
// The load balancer of the docker provider checks the API server with an
// anonymous GET /healthz, which k0s rejects. The test gives it a template that
// checks the backends over TCP instead.
//
// The kubeadm node is a kubeadm-node-join container, started directly with
// docker for the same reasons as in the capi-controlplane-kubeadm-remote-machine
// test. It joins through the load balancer, on the "kind" docker network that
// the controller is on as well.
package kubeadmworkerjoin

import (
	"bytes"
	"context"
	"fmt"
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
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	clusterName   = "kubeadm-join-test"
	nodeContainer = "kubeadm-join-test-node"
	nodeName      = "kubeadm-worker"
	nodeImage     = "kubeadm-node-join"
	dockerNetwork = "kind"

	// The k0s version of the control plane, can be overridden with
	// K0S_CP_VERSION. The kubelet of the kubeadm node must not be newer than
	// its Kubernetes version, nor more than three minor versions older.
	defaultK0sVersion = "v1.33.3+k0s.0"

	tokenID     = "abcdef"
	tokenSecret = "0123456789abcdef"

	// The group that kubeadm gives to bootstrap tokens used for joining nodes.
	kubeadmBootstrapGroup = "system:bootstrappers:kubeadm:default-node-token"

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
kind: Secret
metadata:
  name: bootstrap-token-` + tokenID + `
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: ` + tokenID + `
  token-secret: ` + tokenSecret + `
  usage-bootstrap-authentication: "true"
  auth-extra-groups: ` + kubeadmBootstrapGroup + `
---
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
  name: ` + kubeadmBootstrapGroup + `
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
  name: ` + kubeadmBootstrapGroup + `
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
  name: ` + kubeadmBootstrapGroup + `
---
# Bootstrap tokens may request certificates, which get approved automatically.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:kubelet-bootstrap
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: ` + kubeadmBootstrapGroup + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:node-autoapprove-bootstrap
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: ` + kubeadmBootstrapGroup + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:node-autoapprove-certificate-rotation
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:nodes
`

	// The default HAProxy template of the docker provider, with a TCP check of
	// the backends instead of an anonymous GET /healthz, which k0s rejects with
	// a 401 and would take them out of the load balancer.
	haproxyConfig = `
global
  log /dev/log local0
  log /dev/log local1 notice
  daemon
  maxconn 100000

resolvers docker
  nameserver dns 127.0.0.11:53

defaults
  log global
  mode tcp
  option dontlognull
  timeout connect 5000
  timeout client 50000
  timeout server 50000
  default-server init-addr none

frontend control-plane
  bind *:{{ .FrontendControlPlanePort }}
  default_backend kube-apiservers

backend kube-apiservers
  {{range $server, $backend := .BackendServers}}
  server {{ $server }} {{ JoinHostPort $backend.Address $.BackendControlPlanePort }} weight {{ $backend.Weight }} check resolvers docker resolve-prefer ipv4
  {{- end}}
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
    kind: K0sControlPlane
    name: ` + clusterName + `
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
    kind: DevCluster
    name: ` + clusterName + `
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: DevMachineTemplate
metadata:
  name: ` + clusterName + `-cp-template
  namespace: default
spec:
  template:
    spec:
      backend:
        docker:
          customImage: kindest/node:v1.31.0
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: K0sControlPlane
metadata:
  name: ` + clusterName + `
  namespace: default
spec:
  replicas: 1
  version: {{ .K0sVersion }}
  k0sConfigSpec:
    args:
      - --enable-worker
    k0s:
      apiVersion: k0s.k0sproject.io/v1beta1
      kind: ClusterConfig
      metadata:
        name: k0s
      spec:
        telemetry:
          enabled: false
    files:
    - path: /var/lib/k0s/manifests/kubeadm-join/join.yaml
      content: |
        {{ indent 8 .JoinManifests }}
  machineTemplate:
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: DevMachineTemplate
        name: ` + clusterName + `-cp-template
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + clusterName + `-lb
  namespace: default
data:
  value: |
    {{ indent 4 .HAProxyConfig }}
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: DevCluster
metadata:
  name: ` + clusterName + `
  namespace: default
spec:
  backend:
    docker:
      loadBalancer:
        customHAProxyConfigTemplateRef:
          name: ` + clusterName + `-lb
`))

type KubeadmWorkerJoinSuite struct {
	suite.Suite

	ctx              context.Context
	client           *kubernetes.Clientset
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
	s.client, err = kubernetes.NewForConfig(restCfg)
	s.Require().NoError(err)

	// The manifests are templates themselves, for the Kubernetes version.
	manifests, err := template.New("manifests").Parse(joinManifests)
	s.Require().NoError(err)
	var renderedManifests bytes.Buffer
	s.Require().NoError(manifests.Execute(&renderedManifests, struct{ KubernetesVersion string }{kubernetesVersion}))

	var yaml bytes.Buffer
	s.Require().NoError(clusterYaml.Execute(&yaml, struct{ K0sVersion, JoinManifests, HAProxyConfig string }{
		K0sVersion:    k0sVersion,
		JoinManifests: renderedManifests.String(),
		HAProxyConfig: haproxyConfig,
	}))
	s.clusterYamlsPath = s.T().TempDir() + "/cluster.yaml"
	s.Require().NoError(os.WriteFile(s.clusterYamlsPath, yaml.Bytes(), 0644))

	s.ctx, _ = util.NewSuiteContext(s.T())
}

func (s *KubeadmWorkerJoinSuite) TestKubeadmWorkerJoinsK0sControlPlane() {
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
		s.T().Log("removing the node container")
		_ = exec.Command("docker", "rm", "-f", nodeContainer).Run()
	}()

	var lbPort int
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(_ context.Context) (bool, error) {
		lbPort, _ = getLBPort(clusterName + "-lb")
		return lbPort > 0, nil
	}), "the load balancer of the control plane should come up")

	s.T().Log("waiting for the k0s API")
	k0sConfig, apiServer := s.waitForK0sKubeconfig(ctx, lbPort)
	kc, err := kubernetes.NewForConfig(k0sConfig)
	s.Require().NoError(err)

	s.T().Log("waiting for k0s to apply the bootstrap token")
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		_, err := kc.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, "bootstrap-token-"+tokenID, metav1.GetOptions{})
		if err != nil {
			s.T().Logf("bootstrap token not there yet: %v", err)
			return false, nil
		}
		return true, nil
	}), "k0s should apply the bootstrap token")

	s.T().Log("starting the kubeadm node container")
	s.startNodeContainer(ctx)
	s.prepareNode(ctx)

	s.T().Log("checking that the API server rejects anonymous requests")
	s.Equal(fmt.Sprint(http.StatusUnauthorized), s.anonymousStatus(ctx, apiServer, "/api/v1/namespaces/kube-public/configmaps/cluster-info"))

	discoveryConfig, err := discoveryKubeconfig(apiServer, k0sConfig.CAData)
	s.Require().NoError(err)
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
		return hasIssuedKubeletCSR(ctx, kc), nil
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

// waitForK0sKubeconfig waits for the admin kubeconfig that Cluster API creates
// for the k0s cluster and returns its client configuration, pointing to the
// load balancer's published port as the capi-* tests do. It also returns the
// original server of the kubeconfig, the load balancer's address on the docker
// network.
func (s *KubeadmWorkerJoinSuite) waitForK0sKubeconfig(ctx context.Context, lbPort int) (*rest.Config, string) {
	secretName := clusterName + "-kubeconfig"
	s.Require().NoError(util.WaitForSecret(ctx, s.client, secretName, "default"))
	secret, err := s.client.CoreV1().Secrets("default").Get(ctx, secretName, metav1.GetOptions{})
	s.Require().NoError(err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	s.Require().NoError(err)
	apiServer := cfg.Host
	cfg.Host = fmt.Sprintf("localhost:%d", lbPort)
	return cfg, apiServer
}

// discoveryKubeconfig returns the kubeconfig for kubeadm's file discovery: the
// cluster CA, and the bootstrap token as credentials, which kubeadm then uses
// for the TLS bootstrap of the kubelet as well.
func discoveryKubeconfig(apiServer string, caData []byte) ([]byte, error) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   apiServer,
		CertificateAuthorityData: caData,
	}
	cfg.AuthInfos["tls-bootstrap-token-user"] = &clientcmdapi.AuthInfo{
		Token: tokenID + "." + tokenSecret,
	}
	cfg.Contexts["default"] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: "tls-bootstrap-token-user",
	}
	cfg.CurrentContext = "default"
	return clientcmd.Write(*cfg)
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
func hasIssuedKubeletCSR(ctx context.Context, kc kubernetes.Interface) bool {
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

// getLBPort returns the host port that the load balancer container publishes
// for the API server.
func getLBPort(name string) (int, error) {
	out, err := exec.Command("docker", "inspect", name, "--format",
		`{{with index .NetworkSettings.Ports "6443/tcp"}}{{(index . 0).HostPort}}{{end}}`).Output()
	if err != nil {
		return 0, fmt.Errorf("failed to inspect container %s: %w", name, err)
	}
	var port int
	_, err = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &port)
	return port, err
}
