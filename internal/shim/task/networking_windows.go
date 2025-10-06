package task

import (
	"context"

	"github.com/containerd/nerdbox/internal/shim/task/bundle"
	"github.com/containerd/nerdbox/internal/vm"
)

type networkProvider struct{}

func (p *networksProvider) FromBundle(ctx context.Context, b *bundle.Bundle) error {
	return nil
}

func (p *networksProvider) SetupVM(ctx context.Context, vmi vm.Instance) error {
	return nil
}

func (p *networksProvider) InitArgs() []string {
	return nil
}
