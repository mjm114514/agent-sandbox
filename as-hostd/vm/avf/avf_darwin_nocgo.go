//go:build darwin && !cgo

package avf

import (
	"fmt"
	"log"

	vmapi "github.com/anthropics/agent-sandbox/as-hostd/vm"
)

type Backend struct {
	BootDir string
}

func New(bootDir string) *Backend {
	log.Println("avf backend disabled: as-hostd was built with CGO_ENABLED=0; rebuild with CGO_ENABLED=1 to talk to Virtualization.framework")
	return &Backend{BootDir: bootDir}
}

func (b *Backend) Available() bool { return false }

func (b *Backend) Create(cfg vmapi.Config) (vmapi.VM, error) {
	return nil, fmt.Errorf("avf backend requires cgo (rebuild with CGO_ENABLED=1)")
}
