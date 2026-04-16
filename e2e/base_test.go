package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k0sproject/k0smotron/e2e/util"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	capiutil "sigs.k8s.io/cluster-api/util"
)

func TestBase(t *testing.T) {
	setupAndRun(t, baseSpec)
}

func baseSpec(t *testing.T) {
	require.NotEmpty(t, flavor, "a flavor between Ingress, RemoteHCP or Ignition needs to be specified for this test")

	testName := fmt.Sprintf("%s-%s", "base", strings.ToLower(flavor))

	// Setup a Namespace where to host objects for this spec and create a watcher for the namespace events.
	namespace, _ := util.SetupSpecNamespace(ctx, testName, bootstrapClusterProxy, artifactFolder)

	clusterName := fmt.Sprintf("%s-%s", testName, capiutil.RandomString(6))

	clusterVariables := map[string]string{
		"CLUSTER_NAME": clusterName,
		"NAMESPACE":    namespace.Name,
	}

	flavorValidator, err := getFlavorValidator(flavor)
	require.NoError(t, err, "Failed to get flavor validator")

	flavorClusterVariables, err := flavorValidator.PreClusterInitialization()
	require.NoError(t, err, "Failed during pre-cluster initialization")

	defer func() {
		flavorValidator.PostClusterDeletion(t, &types.NamespacedName{
			Name:      clusterName,
			Namespace: namespace.Name,
		})
	}()

	for k, v := range flavorClusterVariables {
		clusterVariables[k] = v
	}

	infrastructureProvider := "docker"
	if flavor == "Ignition" {
		// Ignition test needs to use the AWS infrastructure provider as CAPD doesn't support ignition scenarios
		infrastructureProvider = "aws"
	}

	// Create a cluster with ingress configuration
	workloadClusterTemplate := clusterctl.ConfigCluster(ctx, clusterctl.ConfigClusterInput{
		ClusterctlConfigPath:     clusterctlConfigPath,
		KubeconfigPath:           bootstrapClusterProxy.GetKubeconfigPath(),
		InfrastructureProvider:   infrastructureProvider,
		Flavor:                   strings.ToLower(flavor),
		Namespace:                namespace.Name,
		ClusterName:              clusterName,
		KubernetesVersion:        e2eConfig.MustGetVariable(KubernetesVersion),
		ControlPlaneMachineCount: ptr.To[int64](1),
		LogFolder:                filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
		ClusterctlVariables:      clusterVariables,
	})

	fmt.Println(string(workloadClusterTemplate))

	// Apply the cluster template yaml
	require.Eventually(t, func() bool {
		return bootstrapClusterProxy.CreateOrUpdate(ctx, workloadClusterTemplate) == nil
	}, 10*time.Second, 1*time.Second, "Failed to apply the cluster template")

	cluster, err := util.DiscoveryAndWaitForCluster(ctx, capiframework.DiscoveryAndWaitForClusterInput{
		Getter:    bootstrapClusterProxy.GetClient(),
		Namespace: namespace.Name,
		Name:      clusterName,
	}, util.GetInterval(e2eConfig, testName, "wait-cluster"))
	require.NoError(t, err)

	defer func() {
		util.DumpSpecResourcesAndCleanup(
			ctx,
			testName,
			bootstrapClusterProxy,
			artifactFolder,
			namespace,
			cancelWatches,
			cluster,
			util.GetInterval(e2eConfig, testName, "wait-delete-cluster"),
			skipCleanup,
			clusterctlConfigPath,
		)
	}()

	_, err = util.DiscoveryAndWaitForControlPlaneInitialized(ctx, capiframework.DiscoveryAndWaitForControlPlaneInitializedInput{
		Lister:  bootstrapClusterProxy.GetClient(),
		Cluster: cluster,
	}, util.GetInterval(e2eConfig, testName, "wait-controllers"))
	require.NoError(t, err)
	t.Log("Control plane is initialized")

	err = util.WaitForMachineDeploymentToBeReady(ctx, capiframework.GetMachineDeploymentsByClusterInput{
		Lister:      bootstrapClusterProxy.GetClient(),
		ClusterName: clusterName,
		Namespace:   namespace.Name,
	}, util.GetInterval(e2eConfig, testName, "wait-workers"))
	require.NoError(t, err)
	t.Log("Workers are ready")

	flavorValidator.Validate(t, cluster)

	t.Log("ALL GOOD!")
}

func getFlavorValidator(flavor string) (FlavorValidator, error) {
	switch flavor {
	case "Ingress":
		return &IngressValidator{}, nil
	case "RemoteHCP":
		return &RemoteHCPValidator{}, nil
	case "Ignition":
		return &IgnitionValidator{}, nil
	default:
		return nil, fmt.Errorf("unknown flavor: %s", flavor)
	}
}
