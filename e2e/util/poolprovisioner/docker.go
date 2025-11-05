package poolprovisioner

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type DockerProvisioner struct{}

func (d *DockerProvisioner) Provision(ctx context.Context, replicas int, nodeVersion string) ([]PooledMachine, error) {
	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %v", err)
	}
	defer apiClient.Close()

	pooledMachines := []PooledMachine{}
	hostConfig := &container.HostConfig{
		Privileged: true,
	}
	for i := range replicas {
		resp, err := apiClient.ContainerCreate(ctx, &container.Config{
			Image: fmt.Sprintf("kindest/node:%s", nodeVersion),
			Tty:   true,
		}, hostConfig, nil, nil, fmt.Sprintf("remote-machine-%d", i))
		if err != nil {
			return nil, fmt.Errorf("create container: %v", err)
		}

		if err := apiClient.ContainerStart(ctx, resp.ID, dockercontainer.StartOptions{}); err != nil {
			return nil, fmt.Errorf("start container: %v", err)
		}

		inspected, err := apiClient.ContainerInspect(ctx, resp.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect container: %v", err)
		}

		pooledMachines = append(pooledMachines, PooledMachine{
			IP:        inspected.NetworkSettings.IPAddress,
			SSHKeyRef: fmt.Sprintf("ssh-key-%d", i),
		})
	}

	return pooledMachines, nil
}

func (d *DockerProvisioner) Clean(ctx context.Context) error {
	return nil
}
