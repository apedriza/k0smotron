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
// Everything that kubeadm expects to find is provisioned through the
// K0sControlPlane only: the manifests that k0s applies from its manifests
// directory (bootstrap token, kubeadm-config, kubelet-config and the RBAC for
// them), the anonymous authentication that lets kubeadm read the cluster-info,
// and a post-start script that publishes the cluster-info once the CA exists.
// The bootstrap signer of the kube-controller-manager of k0s signs it.
//
// The kubeadm node is a kubeadm-node-join container, started directly with
// docker for the same reasons as in the capi-controlplane-kubeadm-remote-machine
// test. It joins through the address that the cluster-info publishes, on the
// "kind" docker network that the controller is on as well.
package kubeadmworkerjoin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
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
  bootstrapToken:
    token: %[1]s
    apiServerEndpoint: %[2]s
    caCertHashes:
    - %[3]s
nodeRegistration:
  name: %[4]s
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

	// What kubeadm reads while joining, as manifests for k0s to apply. The
	// cluster-info is not part of it, since it needs the CA that k0s creates.
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
  usage-bootstrap-signing: "true"
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
# The anonymous read of the cluster-info.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubeadm:bootstrap-signer-clusterinfo
  namespace: kube-public
rules:
- apiGroups: [""]
  resources: [configmaps]
  resourceNames: [cluster-info]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kubeadm:bootstrap-signer-clusterinfo
  namespace: kube-public
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:bootstrap-signer-clusterinfo
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: system:anonymous
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

	// Publishes the cluster-info as kubeadm does, once k0s is up: the cluster CA
	// and the address of the controller, without credentials. The bootstrap
	// signer adds the signature for the bootstrap token.
	//
	// The infrastructure provider runs post-start commands with a deadline, so
	// the script waits for k0s in the background instead of blocking the
	// command. Its output goes to /tmp/publish-cluster-info.log.
	publishClusterInfo = `
cat > /tmp/publish-cluster-info.sh <<'SCRIPT'
set -x
# The admin kubeconfig may point to the external address of the cluster, which
# doesn't reach this controller yet. Talk to the local API server instead, with
# the same credentials. The --server flag would drop them. The admin kubeconfig
# doesn't exist until k0s has started, so it's copied on every attempt.
KUBECTL="k0s kubectl --kubeconfig=/tmp/admin-local.conf"
until sed 's|server: .*|server: https://127.0.0.1:6443|' /var/lib/k0s/pki/admin.conf > /tmp/admin-local.conf && $KUBECTL get namespace kube-public; do sleep 2; done
CA=$(base64 -w0 /var/lib/k0s/pki/ca.crt)
ADDRESS=$(hostname -I | awk '{print $1}')
cat <<EOF | $KUBECTL apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-info
  namespace: kube-public
data:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: $CA
        server: https://$ADDRESS:6443
      name: ""
EOF
SCRIPT
nohup sh /tmp/publish-cluster-info.sh >/tmp/publish-cluster-info.log 2>&1 &
`
)

// Anonymous authentication has to be enabled for the cluster-info to be read
// without credentials. Unlike kubeadm, which only restricts it with RBAC, this
// opens the authentication for all paths, and RBAC keeps them closed.
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
        api:
          extraArgs:
            anonymous-auth: "true"
        telemetry:
          enabled: false
    files:
    - path: /var/lib/k0s/manifests/kubeadm-join/join.yaml
      content: |
        {{ indent 8 .JoinManifests }}
    postStartCommands:
    - |
      {{ indent 6 .PublishClusterInfo }}
  machineTemplate:
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: DevMachineTemplate
        name: ` + clusterName + `-cp-template
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
	s.Require().NoError(clusterYaml.Execute(&yaml, struct{ K0sVersion, JoinManifests, PublishClusterInfo string }{
		K0sVersion:         k0sVersion,
		JoinManifests:      renderedManifests.String(),
		PublishClusterInfo: publishClusterInfo,
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
	k0sConfig := s.waitForK0sKubeconfig(ctx, lbPort)
	kc, err := kubernetes.NewForConfig(k0sConfig)
	s.Require().NoError(err)

	s.T().Log("waiting for the signed cluster-info")
	var apiEndpoint string
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		clusterInfo, err := kc.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "cluster-info", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if _, signed := clusterInfo.Data["jws-kubeconfig-"+tokenID]; !signed {
			return false, nil
		}
		kubeconfig, err := clientcmd.Load([]byte(clusterInfo.Data["kubeconfig"]))
		if err != nil || len(kubeconfig.Clusters) != 1 {
			return false, fmt.Errorf("unexpected cluster-info kubeconfig: %v", err)
		}
		for _, cluster := range kubeconfig.Clusters {
			apiEndpoint = strings.TrimPrefix(cluster.Server, "https://")
		}
		return true, nil
	}), "the cluster-info should be published and signed")
	s.T().Logf("the cluster-info publishes %s", apiEndpoint)

	caHash, err := caCertHash(k0sConfig.CAData)
	s.Require().NoError(err)

	s.T().Log("starting the kubeadm node container")
	s.startNodeContainer(ctx)
	s.prepareNode(ctx)

	s.T().Log("checking that anonymous requests get the cluster-info, and little else")
	s.Equal(fmt.Sprint(http.StatusOK), s.anonymousStatus(ctx, apiEndpoint, "/api/v1/namespaces/kube-public/configmaps/cluster-info"))
	s.Equal(fmt.Sprint(http.StatusForbidden), s.anonymousStatus(ctx, apiEndpoint, "/api/v1/namespaces/kube-system/configmaps/kubeadm-config"))
	s.Equal(fmt.Sprint(http.StatusForbidden), s.anonymousStatus(ctx, apiEndpoint, "/api/v1/nodes"))

	s.T().Logf("running kubeadm join against %s", apiEndpoint)
	joinConfig := fmt.Sprintf(joinConfiguration, tokenID+"."+tokenSecret, apiEndpoint, caHash, nodeName)
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
// load balancer's published port as the capi-* tests do.
func (s *KubeadmWorkerJoinSuite) waitForK0sKubeconfig(ctx context.Context, lbPort int) *rest.Config {
	secretName := clusterName + "-kubeconfig"
	s.Require().NoError(util.WaitForSecret(ctx, s.client, secretName, "default"))
	secret, err := s.client.CoreV1().Secrets("default").Get(ctx, secretName, metav1.GetOptions{})
	s.Require().NoError(err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	s.Require().NoError(err)
	cfg.Host = fmt.Sprintf("localhost:%d", lbPort)
	return cfg
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

// anonymousStatus requests the path from the node container without
// credentials, and returns the HTTP status code.
func (s *KubeadmWorkerJoinSuite) anonymousStatus(ctx context.Context, endpoint, path string) string {
	out, err := exec.CommandContext(ctx, "docker", "exec", nodeContainer,
		"curl", "-sk", "-o", "/dev/null", "-w", "%{http_code}", "https://"+endpoint+path).Output()
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

// caCertHash returns the hash that `kubeadm join` pins the cluster CA with:
// the SHA-256 of the CA certificate's Subject Public Key Info.
func caCertHash(caPEM []byte) (string, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM data in the cluster CA")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse the cluster CA: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
