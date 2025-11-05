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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	cpv1beta1 "github.com/k0sproject/k0smotron/api/controlplane/v1beta1"
	"github.com/k0sproject/k0smotron/e2e/mothership"
	"github.com/k0sproject/k0smotron/e2e/util"
	"github.com/k0sproject/k0smotron/e2e/util/poolprovisioner"
	"sigs.k8s.io/cluster-api/test/framework"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Test suite constants for e2e config variables.
const (
	KubernetesVersion                = "KUBERNETES_VERSION"
	KubernetesVersionManagement      = "KUBERNETES_VERSION_MANAGEMENT"
	KubernetesVersionFirstUpgradeTo  = "KUBERNETES_VERSION_FIRST_UPGRADE_TO"
	KubernetesVersionSecondUpgradeTo = "KUBERNETES_VERSION_SECOND_UPGRADE_TO"
	ControlPlaneMachineCount         = "CONTROL_PLANE_MACHINE_COUNT"
	IPFamily                         = "IP_FAMILY"
	SSHPublicKey                     = "SSH_PUBLIC_KEY"
	RemoteMachineProvisioner         = "REMOTE_MACHINE_PROVISIONER"
	PoolProvisioner                  = "POOL_PROVISIONER"
)

var (
	ctx = ctrl.SetupSignalHandler()

	// watchesCtx is used in log streaming to be able to get canceled via cancelWatches after ending the test suite.
	watchesCtx, cancelWatches = context.WithCancel(ctx)

	// configPath is the path to the e2e config file.
	configPath string

	// e2eConfig to be used for this test, read from configPath.
	e2eConfig *clusterctl.E2EConfig

	// clusterctlConfig is the file which tests will use as a clusterctl config.
	clusterctlConfig string

	// useExistingCluster instructs the test to use the current cluster instead of creating a new one (default discovery rules apply).
	useExistingCluster bool

	// clusterctlConfigPath to be used for this test, created by generating a clusterctl local repository
	// with the providers specified in the configPath.
	clusterctlConfigPath string

	// artifactFolder is the folder to store e2e test artifacts.
	artifactFolder string

	// skipCleanup prevents cleanup of test resources e.g. for debug purposes.
	skipCleanup bool

	// managementClusterProvider manages provisioning of the bootstrap cluster to be used for the e2e tests.
	// Please note that provisioning will be skipped if e2e.use-existing-cluster is provided.
	bootstrapClusterProvider bootstrap.ClusterProvider

	// managementClusterProxy allows to interact with the management cluster to be used for the e2e tests.
	bootstrapClusterProxy capiframework.ClusterProxy

	// infraProvider is the infrastructure provider to be used for the tests. Default is "docker".
	infraProvider string

	// flavor is the flavor to be used for the cluster templates.
	flavor string

	// controlPlaneMachineCount is the number of control plane machines.
	controlPlaneMachineCount int64

	// workerMachineCount is the number of worker machines.
	workerMachineCount int64

	// customClusterctlVariables allows to pass custom variables to clusterctl when creating clusters.
	customClusterctlVariables map[string]string
)

func init() {

	flag.StringVar(&configPath, "config", "", "path to the e2e config file")
	flag.BoolVar(&skipCleanup, "skip-resource-cleanup", false, "if true, the resource cleanup after tests will be skipped")
	flag.StringVar(&artifactFolder, "artifacts-folder", "", "folder where e2e test artifact should be stored")
	flag.BoolVar(&useExistingCluster, "use-existing-cluster", false, "if true, the test uses the current cluster instead of creating a new one (default discovery rules apply)")
	flag.StringVar(&infraProvider, "infra-provider", "docker", "infrastructure provider to be used for the tests")
	flag.StringVar(&flavor, "flavor", "", "the flavor to be used for the cluster templates")
	flag.Int64Var(&controlPlaneMachineCount, "control-plane-machine-count", 3, "the number of control plane machines")
	flag.Int64Var(&workerMachineCount, "worker-machine-count", 0, "the number of worker machines")

	// On the k0smotron side we avoid using Gomega for assertions but since we want to use the
	// cluster-api framework as much as possible, the framework assertions require registering
	// a fail handler beforehand.
	gomega.RegisterFailHandler(func(message string, callerSkip ...int) {
		panic(message)
	})
}

type specInput struct {
	infraProvider             string
	flavor                    string
	controlPlaneMachineCount  int64
	workerMachineCount        int64
	customClusterctlVariables map[string]string
}

func setupAndRun(t *testing.T, test func(t *testing.T, input specInput)) {
	ctrl.SetLogger(klog.Background())
	flag.Parse()

	defer func() {
		if !skipCleanup {
			tearDown(bootstrapClusterProvider, bootstrapClusterProxy)

			// if infraProvider == "k0sproject-k0smotron" {
			// 	err := poolprovisioner.GetProvisioner().Clean(ctx)
			// 	if err != nil {
			// 		fmt.Printf("Failed to clean machine pool: %v\n", err)
			// 	}
			// }
		}
	}()
	err := setupMothership()
	if err != nil {
		panic(err)
	}

	input := specInput{
		infraProvider:             infraProvider,
		flavor:                    flavor,
		controlPlaneMachineCount:  controlPlaneMachineCount,
		workerMachineCount:        workerMachineCount,
		customClusterctlVariables: map[string]string{},
	}

	// If using k0smotron as infra provider, create a virtual machine pool for the infrastructure to be used
	// for the remote machines.
	if infraProvider == "k0sproject-k0smotron" {
		if input.flavor == "" {
			// Default flavor for k0sproject-k0smotron infrastructure provider is "ssh"
			input.flavor = "ssh"
		}

		pooledMachines, err := createMachinePool(ctx, controlPlaneMachineCount+workerMachineCount, "v1.32.0")
		if err != nil {
			panic(fmt.Errorf("failed to create machine pool: %w", err))
		}

		machinesPoolTemplate, err := generateMachinePoolTemplate(pooledMachines)
		if err != nil {
			panic(fmt.Errorf("failed to create machine pool template: %w", err))
		}

		// append template to the template used for the tests
		// maybe pool machine variable is not needed.

		input.customClusterctlVariables["POOL_MACHINES"] = string(machinesPoolTemplate)
	}

	test(t, input)
}

func setupMothership() error {
	var err error
	e2eConfig, err = loadE2EConfig(ctx, configPath)
	if err != nil {
		return fmt.Errorf("failed to load e2e config: %w", err)
	}

	if clusterctlConfig == "" {
		clusterctlConfigPath = clusterctl.CreateRepository(ctx, clusterctl.CreateRepositoryInput{
			E2EConfig:        e2eConfig,
			RepositoryFolder: filepath.Join(artifactFolder, "repository"),
		})
	} else {
		clusterctlConfigPath = clusterctlConfig
	}

	scheme, err := initScheme()
	if err != nil {
		return err
	}

	kubeconfigPath := ""
	if !useExistingCluster {
		bootstrapClusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
			Name:               e2eConfig.ManagementClusterName,
			Images:             e2eConfig.Images,
			KubernetesVersion:  e2eConfig.MustGetVariable(KubernetesVersionManagement),
			RequiresDockerSock: e2eConfig.HasDockerProvider(),
			IPFamily:           e2eConfig.MustGetVariable(IPFamily),
			LogFolder:          filepath.Join(artifactFolder, "kind"),
		})
		if bootstrapClusterProvider == nil {
			return errors.New("failed to create a management cluster")
		}
		kubeconfigPath = bootstrapClusterProvider.GetKubeconfigPath()
	} else {
		fmt.Println("Using an existing bootstrap cluster")
	}

	bootstrapClusterProxy = capiframework.NewClusterProxy("bootstrap", kubeconfigPath, scheme, framework.WithMachineLogCollector(framework.DockerLogCollector{}))
	if bootstrapClusterProxy == nil {
		return errors.New("failed to get a management cluster proxy")
	}

	err = mothership.InitAndWatchControllerLogs(watchesCtx, clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:             bootstrapClusterProxy,
		ClusterctlConfigPath:     clusterctlConfigPath,
		InfrastructureProviders:  e2eConfig.InfrastructureProviders(),
		DisableMetricsCollection: true,
		BootstrapProviders:       []string{"k0sproject-k0smotron"},
		ControlPlaneProviders:    []string{"k0sproject-k0smotron"},
		LogFolder:                filepath.Join(artifactFolder, "capi"),
	}, util.GetInterval(e2eConfig, "bootstrap", "wait-deployment-available"))
	if err != nil {
		return fmt.Errorf("failed to init management cluster: %w", err)
	}

	return nil
}

func createMachinePool(ctx context.Context, replicas int64, nodeVersion string) ([]poolprovisioner.PooledMachine, error) {
	var pp poolprovisioner.Provisioner
	switch os.Getenv(PoolProvisioner) {
	case "docker", "":
		pp = &poolprovisioner.DockerProvisioner{}
	// TODO: add AWS as provisioner
	default:
		return nil, fmt.Errorf("unknown pool provisioner: %s", os.Getenv(PoolProvisioner))
	}

	return pp.Provision(ctx, int(replicas), nodeVersion)
}

func generateMachinePoolTemplate(pooledMachines []poolprovisioner.PooledMachine) ([]byte, error) {
	poolMachineTemplate := `apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PooledRemoteMachine
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${NAMESPACE}
spec:
  pool: default
  machine:
	address: %s
	port: 22
	user: root
	sshKeyRef:
	  name: %s
`
	pooledMachinesStrings := []string{}
	for _, pm := range pooledMachines {
		pooledMachinesStrings = append(pooledMachinesStrings, fmt.Sprintf(poolMachineTemplate, pm.IP, pm.SSHKeyRef))
	}

	return nil, nil
}

func tearDown(bootstrapClusterProvider bootstrap.ClusterProvider, bootstrapClusterProxy framework.ClusterProxy) {
	cancelWatches()
	if bootstrapClusterProxy != nil {
		bootstrapClusterProxy.Dispose(ctx)
	}
	if bootstrapClusterProvider != nil {
		bootstrapClusterProvider.Dispose(ctx)
	}
}

func initScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	capiframework.TryAddDefaultSchemes(s)
	err := cpv1beta1.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func loadE2EConfig(ctx context.Context, configPath string) (*clusterctl.E2EConfig, error) {
	configData, err := os.ReadFile(configPath)

	if err != nil {
		return nil, fmt.Errorf("failed to read the e2e test config file: %w", err)
	}

	config := &clusterctl.E2EConfig{}
	err = yaml.Unmarshal(configData, config)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the e2e test config file to yaml: %w", err)
	}

	err = config.ResolveReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve release markers in e2e test config file: %w", err)
	}

	config.Defaults()
	config.AbsPaths(filepath.Dir(configPath))

	return config, nil
}
