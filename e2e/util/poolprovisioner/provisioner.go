package poolprovisioner

import "context"

type PooledMachine struct {
	IP        string
	SSHKeyRef string
}

type Provisioner interface {
	Provision(ctx context.Context, replicas int, nodeVersion string) ([]PooledMachine, error)
	Clean(ctx context.Context) error
}
