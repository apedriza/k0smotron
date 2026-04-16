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
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/test/framework"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/util/secret"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

const (
	hostingClusterName = "hosting-cluster"
)

var (
	hostingClusterProxy capiframework.ClusterProxy
)

type RemoteHCPValidator struct{}

func (v *RemoteHCPValidator) PreClusterInitialization() (map[string]string, error) {
	deployHostingCluster()
	encodedHostingClusterKubeconfig, err := getEncodedHostingClusterKubeconfig()
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"HOSTING_CLUSTER_KUBECONFIG": encodedHostingClusterKubeconfig,
	}, nil
}

func (v *RemoteHCPValidator) Validate(t *testing.T, cluster *clusterv1.Cluster) {
	// noop
}

func (v *RemoteHCPValidator) PostClusterDeletion(t *testing.T, cluster *types.NamespacedName) error {
	t.Log("Waiting for GC of hosting-cluster resources")
	// Wait for external owner to be deleted in hosting cluster
	ownerName := fmt.Sprintf("%s-root-owner", cluster.Name)

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 1*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		cm := &corev1.ConfigMap{}
		err := hostingClusterProxy.GetClient().Get(ctx, crclient.ObjectKey{Namespace: cluster.Namespace, Name: ownerName}, cm)
		return apierrors.IsNotFound(err), nil
	}))

	// Ensure etcd cert Secrets are garbage collected in hosting cluster
	for _, name := range []string{
		secret.Name(cluster.Name, secret.APIServerEtcdClient),
		secret.Name(cluster.Name, "etcd-server"),
		secret.Name(cluster.Name, "etcd-peer"),
	} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			sec := &corev1.Secret{}
			err := hostingClusterProxy.GetClient().Get(ctx, crclient.ObjectKey{Namespace: cluster.Namespace, Name: name}, sec)
			return apierrors.IsNotFound(err), nil
		}))
	}

	// Optionally ensure key workload resources are GC'd as well (etcd StatefulSet & Service)
	etcdStsName := fmt.Sprintf("kmc-%s-etcd", cluster.Name)
	etcdSvcName := fmt.Sprintf("kmc-%s-etcd", cluster.Name)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		sts := &appsv1.StatefulSet{}
		err := hostingClusterProxy.GetClient().Get(ctx, crclient.ObjectKey{Namespace: cluster.Namespace, Name: etcdStsName}, sts)
		return apierrors.IsNotFound(err), nil
	}))
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		svc := &corev1.Service{}
		err := hostingClusterProxy.GetClient().Get(ctx, crclient.ObjectKey{Namespace: cluster.Namespace, Name: etcdSvcName}, svc)
		return apierrors.IsNotFound(err), nil
	}))

	return deleteHostingcluster()
}

// deployHostingCluster deploys a cluster for hosting control planes.
// TODO: Currently the cluster is based on Kind. An alternative would be to create a workload cluster and convert it into the hosting cluster.
// In that proposed approach, the infra of the hosting cluster is configurable.
func deployHostingCluster() {
	hostingClusterProvider := bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
		Name:               hostingClusterName,
		RequiresDockerSock: false,
		IPFamily:           "IPv4",
		LogFolder:          filepath.Join(artifactFolder, "kind"),
		ExtraPortMappings: []v1alpha4.PortMapping{
			{
				ContainerPort: 31443,
				HostPort:      31443,
			},
		},
	})
	if hostingClusterProvider == nil {
		panic("failed to create a management cluster")
	}

	hostingClusterProxy = capiframework.NewClusterProxy("bootstrap", hostingClusterProvider.GetKubeconfigPath(), getHostingClusterDefaultScheme(), framework.WithMachineLogCollector(framework.DockerLogCollector{}))
	if hostingClusterProxy == nil {
		panic("failed to get a management cluster proxy")
	}
}

func deleteHostingcluster() error {
	clusterProvider := cluster.NewProvider()

	// kubeconfig is used to remove the cluster from the host so internal=false in order to use the host IP.
	kubeconfig, err := clusterProvider.KubeConfig(hostingClusterName, false)
	if err != nil {
		return err
	}

	return clusterProvider.Delete(hostingClusterName, kubeconfig)
}

func getEncodedHostingClusterKubeconfig() (string, error) {
	// kubeconfig value will be used by the management cluster to instantiate a remote cluster client so it is needed to set internal=true
	// in order to use the internal IP of the cluster.
	kubeconfig, err := cluster.NewProvider().KubeConfig(hostingClusterName, true)
	if err != nil {
		return "", nil
	}

	return base64.StdEncoding.EncodeToString([]byte(kubeconfig)), nil
}

func getHostingClusterDefaultScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	corev1.AddToScheme(s)
	appsv1.AddToScheme(s)
	rbacv1.AddToScheme(s)
	return s
}
