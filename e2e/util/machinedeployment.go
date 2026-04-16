package util

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiframework "sigs.k8s.io/cluster-api/test/framework"
)

func WaitForMachineDeploymentToBeReady(ctx context.Context, input capiframework.GetMachineDeploymentsByClusterInput, interval Interval) error {
	fmt.Println("Waiting for the control plane to be ready")

	mdl := &clusterv1.MachineDeploymentList{}
	err := wait.PollUntilContextTimeout(ctx, interval.tick, interval.timeout, true, func(ctx context.Context) (done bool, err error) {
		err = input.Lister.List(ctx, mdl, byClusterOptions(input.ClusterName, input.Namespace)...)
		if err != nil {
			return false, fmt.Errorf("error listing MachineDeployments: %w", err)
		}

		if len(mdl.Items) == 0 {
			return false, nil
		}

		md := &mdl.Items[0]

		desiredReplicas := ptr.Deref(md.Spec.Replicas, 0)
		statusReplicas := ptr.Deref(md.Status.Replicas, 0)
		updatedReplicas := ptr.Deref(md.Status.UpToDateReplicas, 0)
		readyReplicas := ptr.Deref(md.Status.ReadyReplicas, 0)
		unavailableReplicas := statusReplicas - readyReplicas

		if statusReplicas != desiredReplicas ||
			updatedReplicas != desiredReplicas ||
			readyReplicas != desiredReplicas ||
			unavailableReplicas > 0 {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return fmt.Errorf(capiframework.PrettyPrint(mdl) + "\n")
	}

	return nil
}
