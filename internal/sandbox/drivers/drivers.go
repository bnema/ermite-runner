// SPDX-License-Identifier: MIT

// Package drivers wires the sandbox drivers into sandbox.Select.
package drivers

import (
	"context"

	"bnema.dev/ermite-runner/internal/sandbox"
	"bnema.dev/ermite-runner/internal/sandbox/container"
	"bnema.dev/ermite-runner/internal/sandbox/vm"
)

// Factories returns the real vm and container driver factories.
func Factories() sandbox.Factories {
	return sandbox.Factories{
		VM: func(ctx context.Context, cfg sandbox.Config) (sandbox.Driver, error) {
			return vm.New(ctx, cfg, vm.Options{})
		},
		Container: func(ctx context.Context, cfg sandbox.Config) (sandbox.Driver, error) {
			return container.New(ctx, cfg, container.Options{})
		},
	}
}
