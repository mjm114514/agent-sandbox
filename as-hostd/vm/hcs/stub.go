//go:build !windows

package hcs

import (
	"fmt"

	vmapi "github.com/anthropics/agent-sandbox/as-hostd/vm"
)

type Backend struct {
	BootDir string
}

func New(bootDir string) *Backend {
	return &Backend{BootDir: bootDir}
}

func (b *Backend) Available() bool { return false }

func (b *Backend) Create(cfg vmapi.Config) (vmapi.VM, error) {
	return nil, fmt.Errorf("hcs backend not supported on this platform")
}
