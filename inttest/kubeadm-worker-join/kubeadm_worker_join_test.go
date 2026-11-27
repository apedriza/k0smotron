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
// join a k0s cluster with a plain `kubeadm join --token ...
// --discovery-token-ca-cert-hash ...`, without any support for it in k0s.
//
// Like the capi-* tests, it runs against an existing kind management cluster
// (KUBECONFIG) that has Cluster API and k0smotron installed. The k0s cluster is
// a K0smotronControlPlane, hosted in the management cluster and exposed with a
// NodePort service, with no workers.
//
// The node uses kubeadm's token discovery, which reads the cluster-info
// anonymously. Anonymous authentication is limited to the cluster-info with an
// AuthenticationConfiguration mounted into the control plane pod. k0s leaves out
// its --anonymous-auth=false default when that configuration has an anonymous
// section, since v1.36.4 and v1.37.1.
//
// What kubeadm reads is provisioned as manifests that k0s applies from its
// manifests directory: kubeadm-config, kubelet-config, and the RBAC for them
// and for the anonymous read of the cluster-info. k0s already lets bootstrap
// tokens request kubelet certificates and approves them.
//
// The test then does what an external provisioner such as NVIDIA DPF does: it
// publishes the cluster-info, creates a bootstrap token in the kubeadm group
// with the signing usage, so that the bootstrap signer of k0s signs the
// cluster-info for it, and runs kubeadm join with the token and the CA hash.
//
// The kubeadm node is a kubeadm-node-join container, started directly with
// docker for the same reasons as in the capi-controlplane-kubeadm-remote-machine
// test. It joins through the NodePort of the control plane, on the "kind"
// docker network that the management cluster is on as well.
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
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
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
	// K0S_CP_VERSION. It has to be at least v1.36.4 or v1.37.1 for k0s to leave
	// anonymous authentication to the authentication configuration. The
	// kubelet of the kubeadm node must not be newer than its Kubernetes
	// version, nor more than three minor versions older.
	defaultK0sVersion = "v1.37.2+k0s.1"

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

	// Lets anonymous requests through only for the cluster-info.
	authenticationConfig = `
apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
anonymous:
  enabled: true
  conditions:
  - path: /api/v1/namespaces/kube-public/configmaps/cluster-info
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
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + clusterName + `-authentication-config
  namespace: default
data:
  authentication-config.yaml: |
    {{ indent 4 .AuthenticationConfig }}
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
  mounts:
  - path: /etc/k0s/authentication-config
    configMap:
      name: ` + clusterName + `-authentication-config
  k0sConfig:
    apiVersion: k0s.k0sproject.io/v1beta1
    kind: ClusterConfig
    spec:
      api:
        extraArgs:
          authentication-config: /etc/k0s/authentication-config/authentication-config.yaml
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
	s.Require().NoError(clusterYaml.Execute(&yaml, struct{ K0sVersion, JoinManifests, AuthenticationConfig string }{
		K0sVersion:           k0sVersion,
		JoinManifests:        renderedManifests.String(),
		AuthenticationConfig: authenticationConfig,
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
	k0sConfig, apiServer := s.waitForK0sKubeconfig(ctx, localPort)
	kc, err := kubernetes.NewForConfig(k0sConfig)
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

	s.T().Logf("publishing the cluster-info for %s and creating a bootstrap token", apiServer)
	s.publishClusterInfo(ctx, kc, apiServer, k0sConfig.CAData)
	s.createBootstrapToken(ctx, kc)

	s.T().Log("waiting for the bootstrap signer to sign the cluster-info")
	s.Require().NoError(wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		clusterInfo, err := kc.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "cluster-info", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		_, signed := clusterInfo.Data["jws-kubeconfig-"+tokenID]
		return signed, nil
	}), "the cluster-info should be signed for the bootstrap token")

	s.T().Log("starting the kubeadm node container")
	s.startNodeContainer(ctx)
	s.prepareNode(ctx)

	s.T().Log("checking that anonymous requests get the cluster-info only")
	s.Equal(fmt.Sprint(http.StatusOK), s.anonymousStatus(ctx, apiServer, "/api/v1/namespaces/kube-public/configmaps/cluster-info"))
	s.Equal(fmt.Sprint(http.StatusUnauthorized), s.anonymousStatus(ctx, apiServer, "/api/v1/namespaces/kube-system/configmaps/kubeadm-config"))
	s.Equal(fmt.Sprint(http.StatusUnauthorized), s.anonymousStatus(ctx, apiServer, "/api/v1/nodes"))

	caHash, err := caCertHash(k0sConfig.CAData)
	s.Require().NoError(err)
	apiEndpoint := strings.TrimPrefix(apiServer, "https://")

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

// waitForK0sKubeconfig waits for the admin kubeconfig that k0smotron creates
// for the k0s cluster and returns its client configuration, pointing to the
// local port forwarded to the control plane pod as the capi-* tests do. It also
// returns the original server of the kubeconfig, the NodePort address that the
// docker network reaches.
func (s *KubeadmWorkerJoinSuite) waitForK0sKubeconfig(ctx context.Context, localPort int) (*rest.Config, string) {
	secretName := clusterName + "-kubeconfig"
	s.Require().NoError(util.WaitForSecret(ctx, s.client, secretName, "default"))
	secret, err := s.client.CoreV1().Secrets("default").Get(ctx, secretName, metav1.GetOptions{})
	s.Require().NoError(err)

	cfg, err := clientcmd.RESTConfigFromKubeConfig(secret.Data["value"])
	s.Require().NoError(err)
	apiServer := cfg.Host
	cfg.Host = fmt.Sprintf("localhost:%d", localPort)
	return cfg, apiServer
}

// publishClusterInfo publishes the cluster-info as kubeadm init does: the
// cluster CA and the API server address, without credentials.
func (s *KubeadmWorkerJoinSuite) publishClusterInfo(ctx context.Context, kc kubernetes.Interface, apiServer string, caData []byte) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[""] = &clientcmdapi.Cluster{
		Server:                   apiServer,
		CertificateAuthorityData: caData,
	}
	kubeconfig, err := clientcmd.Write(*cfg)
	s.Require().NoError(err)

	_, err = kc.CoreV1().ConfigMaps(metav1.NamespacePublic).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: metav1.NamespacePublic},
		Data:       map[string]string{"kubeconfig": string(kubeconfig)},
	}, metav1.CreateOptions{})
	s.Require().NoError(err)
}

// createBootstrapToken creates a bootstrap token as kubeadm token create and
// NVIDIA DPF do: in the kubeadm group, and with the signing usage, so that the
// bootstrap signer signs the cluster-info for it.
func (s *KubeadmWorkerJoinSuite) createBootstrapToken(ctx context.Context, kc kubernetes.Interface) {
	_, err := kc.CoreV1().Secrets(metav1.NamespaceSystem).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-" + tokenID, Namespace: metav1.NamespaceSystem},
		Type:       corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       tokenID,
			"token-secret":                   tokenSecret,
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              kubeadmBootstrapGroup,
		},
	}, metav1.CreateOptions{})
	s.Require().NoError(err)
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
