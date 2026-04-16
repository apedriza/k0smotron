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

package util

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	cpv1beta2 "github.com/k0sproject/k0smotron/api/controlplane/v1beta2"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/util/patch"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func WaitForControlPlaneToBeReady(ctx context.Context, client crclient.Client, cp *unstructured.Unstructured, interval Interval) error {
	fmt.Println("Waiting for the control plane to be ready")

	controlplaneObjectKey := crclient.ObjectKey{
		Name:      cp.GetName(),
		Namespace: cp.GetNamespace(),
	}
	controlplane := &unstructured.Unstructured{}
	err := wait.PollUntilContextTimeout(ctx, interval.tick, interval.timeout, true, func(ctx context.Context) (done bool, err error) {
		if err := client.Get(ctx, controlplaneObjectKey, controlplane); err != nil {
			return false, errors.Wrapf(err, "failed to get controlplane")
		}

		desiredReplicas, _, _ := unstructured.NestedInt64(controlplane.Object, "spec", "replicas")
		statusReplicas, _, _ := unstructured.NestedInt64(controlplane.Object, "status", "replicas")
		updatedReplicas, _, _ := unstructured.NestedInt64(controlplane.Object, "status", "upToDateReplicas")
		readyReplicas, _, _ := unstructured.NestedInt64(controlplane.Object, "status", "readyReplicas")
		availableReplicas, _, _ := unstructured.NestedInt64(controlplane.Object, "status", "availableReplicas")
		unavailableReplicas := desiredReplicas - availableReplicas
		versionInSpec, _, _ := unstructured.NestedString(controlplane.Object, "spec", "version")
		versionInStatus, _, _ := unstructured.NestedString(controlplane.Object, "status", "version")

		if statusReplicas != desiredReplicas ||
			updatedReplicas != desiredReplicas ||
			readyReplicas != desiredReplicas ||
			unavailableReplicas > 0 ||
			versionInSpec != versionInStatus {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf(capiframework.PrettyPrint(controlplane) + "\n")
	}

	return nil
}

// UpgradeControlPlaneAndWaitForUpgradeInput is the input type for UpgradeControlPlaneAndWaitForUpgrade.
type UpgradeControlPlaneAndWaitForUpgradeInput struct {
	GetLister                        capiframework.GetLister
	ClusterProxy                     capiframework.ClusterProxy
	Cluster                          *clusterv1.Cluster
	ControlPlane                     *unstructured.Unstructured
	KubernetesUpgradeVersion         string
	WaitForKubeProxyUpgradeInterval  Interval
	WaitForControlPlaneReadyInterval Interval
}

// UpgradeControlPlaneAndWaitForUpgrade upgrades a K0sControlPlane and waits for it to be upgraded.
func UpgradeControlPlaneAndWaitForReadyUpgrade(ctx context.Context, input UpgradeControlPlaneAndWaitForUpgradeInput) error {
	mgmtClient := input.ClusterProxy.GetClient()

	fmt.Println("Patching the new kubernetes version to KCP")
	patchHelper, err := patch.NewHelper(input.ControlPlane, mgmtClient)
	if err != nil {
		return err
	}

	if err := unstructured.SetNestedField(input.ControlPlane.Object, input.KubernetesUpgradeVersion, "spec", "version"); err != nil {
		return fmt.Errorf("failed to set new kubernetes version to controlplane %s: %w", klog.KObj(input.ControlPlane), err)
	}

	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (done bool, err error) {
		return patchHelper.Patch(ctx, input.ControlPlane) == nil, nil
	})
	if err != nil {
		return fmt.Errorf("failed to patch the new kubernetes version to controlplane %s: %w", klog.KObj(input.ControlPlane), err)
	}

	err = WaitForControlPlaneToBeReady(ctx, input.ClusterProxy.GetClient(), input.ControlPlane, input.WaitForControlPlaneReadyInterval)
	if err != nil {
		return err
	}

	fmt.Println("Waiting for kube-proxy to have the upgraded kubernetes version")
	workloadCluster := input.ClusterProxy.GetWorkloadCluster(ctx, input.Cluster.Namespace, input.Cluster.Name)
	workloadClient := workloadCluster.GetClient()
	return WaitForKubeProxyUpgrade(ctx, WaitForKubeProxyUpgradeInput{
		Getter:            workloadClient,
		KubernetesVersion: input.KubernetesUpgradeVersion,
	}, input.WaitForKubeProxyUpgradeInterval)
}

func DiscoveryAndWaitForControlPlaneInitialized(ctx context.Context, input capiframework.DiscoveryAndWaitForControlPlaneInitializedInput, interval Interval) (*unstructured.Unstructured, error) {
	var controlPlane *unstructured.Unstructured
	err := wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		controlPlane, err = getControlPlaneByCluster(ctx, GetControlPlaneByClusterInput{
			Lister:      input.Lister,
			ClusterName: input.Cluster.Name,
			Namespace:   input.Cluster.Namespace,
		})
		if err != nil {
			return false, err
		}

		if controlPlane == nil {
			return false, nil
		}

		initialized, found, err := unstructured.NestedBool(
			controlPlane.Object,
			"status",
			"initialization",
			"controlPlaneInitialized",
		)
		if err != nil {
			return false, fmt.Errorf("invalid type for controlPlaneInitialized: %w", err)
		}

		if !found {
			return false, nil
		}

		return initialized, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error waiting for control plane to be initialized for %s: %w", klog.KObj(input.Cluster), err)
	}

	return controlPlane, nil
}

type GetControlPlaneByClusterInput struct {
	Lister      capiframework.Lister
	ClusterName string
	Namespace   string
}

func getControlPlaneByCluster(ctx context.Context, input GetControlPlaneByClusterInput) (*unstructured.Unstructured, error) {
	k0sControlPlaneList := &cpv1beta2.K0sControlPlaneList{}
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (done bool, err error) {
		return input.Lister.List(ctx, k0sControlPlaneList, byClusterOptions(input.ClusterName, input.Namespace)...) == nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list K0sControlPlane object for Cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	}
	if len(k0sControlPlaneList.Items) > 1 {
		return nil, fmt.Errorf("cluster %s should not have more than 1 K0sControlPlane object", klog.KRef(input.Namespace, input.ClusterName))
	}
	if len(k0sControlPlaneList.Items) == 1 {
		objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&k0sControlPlaneList.Items[0])
		if err != nil {
			return nil, err
		}

		unstructuredControlPlane := &unstructured.Unstructured{
			Object: objMap,
		}
		return unstructuredControlPlane, nil
	}

	k0smotronControlPlaneList := &cpv1beta2.K0smotronControlPlaneList{}
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (done bool, err error) {
		return input.Lister.List(ctx, k0smotronControlPlaneList, byClusterOptions(input.ClusterName, input.Namespace)...) == nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list K0smotronControlPlane object for Cluster %s", klog.KRef(input.Namespace, input.ClusterName))
	}
	if len(k0smotronControlPlaneList.Items) > 1 {
		return nil, fmt.Errorf("cluster %s should not have more than 1 K0smotronControlPlane object", klog.KRef(input.Namespace, input.ClusterName))
	}
	if len(k0smotronControlPlaneList.Items) == 1 {
		objMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&k0smotronControlPlaneList.Items[0])
		if err != nil {
			return nil, err
		}

		unstructuredControlPlane := &unstructured.Unstructured{
			Object: objMap,
		}
		return unstructuredControlPlane, nil
	}

	return nil, nil
}

// byClusterOptions returns a set of ListOptions that allows to identify all the objects belonging to a Cluster.
func byClusterOptions(name, namespace string) []crclient.ListOption {
	return []crclient.ListOption{
		crclient.InNamespace(namespace),
		crclient.MatchingLabels{
			clusterv1.ClusterNameLabel: name,
		},
	}
}

type WaitForOneK0sControlPlaneMachineToExistInput struct {
	Lister       capiframework.Lister
	Cluster      *clusterv1.Cluster
	ControlPlane *cpv1beta2.K0sControlPlane
}

type WaitForKubeProxyUpgradeInput struct {
	Getter            capiframework.Getter
	KubernetesVersion string
}

// WaitForKubeProxyUpgrade waits until kube-proxy version matches with the kubernetes version.
func WaitForKubeProxyUpgrade(ctx context.Context, input WaitForKubeProxyUpgradeInput, interval Interval) error {
	fmt.Println("Ensuring kube-proxy has the correct image")

	// this desired version is sticky to the k0s naming on the kube-proxy image
	versionPrefix := strings.Split(input.KubernetesVersion, "+")[0]
	wantKubeProxyImage := fmt.Sprintf("quay.io/k0sproject/kube-proxy:%s", versionPrefix)

	return wait.PollUntilContextTimeout(ctx, interval.tick, interval.timeout, true, func(ctx context.Context) (done bool, err error) {
		ds := &appsv1.DaemonSet{}

		if err := input.Getter.Get(ctx, crclient.ObjectKey{Name: "kube-proxy", Namespace: metav1.NamespaceSystem}, ds); err != nil {
			return false, err
		}

		if strings.HasPrefix(ds.Spec.Template.Spec.Containers[0].Image, wantKubeProxyImage) {
			return true, nil
		}

		return false, nil
	})
}

// K0smotronControlPlane helper functions

func WaitForK0smotronControlPlaneToBeReady(ctx context.Context, client crclient.Client, clusterName, namespace string, interval Interval) error {
	fmt.Println("Waiting for the K0smotronControlPlane to be ready")

	controlplaneObjectKey := crclient.ObjectKey{
		Name:      clusterName,
		Namespace: namespace,
	}

	kcp := &unstructured.Unstructured{}
	kcp.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	kcp.SetKind("K0smotronControlPlane")

	err := wait.PollUntilContextTimeout(ctx, interval.tick, interval.timeout, true, func(ctx context.Context) (done bool, err error) {
		if err := client.Get(ctx, controlplaneObjectKey, kcp); err != nil {
			return false, errors.Wrapf(err, "failed to get K0smotronControlPlane")
		}

		// Check if the control plane is ready
		status, found, err := unstructured.NestedMap(kcp.Object, "status")
		if err != nil || !found {
			return false, nil
		}

		ready, found, err := unstructured.NestedBool(status, "initialization", "controlPlaneInitialized")
		if err != nil || !found {
			return false, nil
		}

		return ready, nil
	})
	if err != nil {
		return fmt.Errorf("K0smotronControlPlane failed to become ready: %w", err)
	}

	return nil
}

func DiscoveryAndWaitForK0smotronControlPlaneInitialized(ctx context.Context, input capiframework.DiscoveryAndWaitForControlPlaneInitializedInput, interval Interval) error {
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (done bool, err error) {
		exists, err := k0smotronControlPlaneExists(ctx, K0smotronControlPlaneExistsInput{
			Lister:      input.Lister,
			ClusterName: input.Cluster.Name,
			Namespace:   input.Cluster.Namespace,
		})
		if err != nil {
			return false, err
		}

		return exists, nil
	})
	if err != nil {
		return fmt.Errorf("couldn't get the K0smotronControlPlane for the cluster %s: %w", klog.KObj(input.Cluster), err)
	}

	fmt.Printf("K0smotronControlPlane found for cluster %s\n", klog.KObj(input.Cluster))
	return nil
}

type K0smotronControlPlaneExistsInput struct {
	Lister      capiframework.Lister
	ClusterName string
	Namespace   string
}

func k0smotronControlPlaneExists(ctx context.Context, input K0smotronControlPlaneExistsInput) (bool, error) {
	kcpList := &unstructured.UnstructuredList{}
	kcpList.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta1")
	kcpList.SetKind("K0smotronControlPlaneList")

	err := input.Lister.List(ctx, kcpList, byClusterOptions(input.ClusterName, input.Namespace)...)
	if err != nil {
		return false, err
	}

	// Check if we found any K0smotronControlPlane objects
	for _, item := range kcpList.Items {
		if item.GetName() == input.ClusterName && item.GetNamespace() == input.Namespace {
			return true, nil
		}
	}

	return false, nil
}
