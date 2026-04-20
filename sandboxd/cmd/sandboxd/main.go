package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/agent-sandbox/sandboxd/netstack"
	"github.com/anthropics/agent-sandbox/sandboxd/rpc"
	"github.com/anthropics/agent-sandbox/sandboxd/vm"
	"github.com/anthropics/agent-sandbox/sandboxd/vm/hcs"
)

type daemon struct {
	backend   vm.Backend
	currentVM vm.VM
	agentConn *rpc.Conn
	sdk       *rpc.StdioServer
	netSvc    *netstack.Service
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(os.Stderr)
	log.Println("sandboxd starting")

	d := &daemon{}
	d.detectBackend()

	sdkServer := rpc.NewStdioServer(os.Stdin, os.Stdout, nil)
	d.sdk = sdkServer
	sdkServer.SetHandler(d.handleSDKCall)

	if err := sdkServer.Serve(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func (d *daemon) detectBackend() {
	// Default boot dir: alongside the sandboxd binary
	bootDir := filepath.Join(filepath.Dir(os.Args[0]), "boot")
	b := hcs.New(bootDir)
	if b.Available() {
		d.backend = b
		log.Printf("using HCS backend, boot dir: %s", bootDir)
		return
	}
	log.Println("no VM backend available")
}

func (d *daemon) handleSDKCall(method string, params json.RawMessage) (any, error) {
	switch method {
	case "sandbox.create":
		return d.sandboxCreate(params)
	case "sandbox.start":
		return d.sandboxStart()
	case "sandbox.stop":
		return d.sandboxStop()
	case "sandbox.destroy":
		return d.sandboxDestroy()
	default:
		if d.agentConn == nil {
			return nil, &rpc.Error{Code: -32601, Message: "vm-agent not connected"}
		}
		result, err := d.agentConn.Call(method, params)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

type createParams struct {
	Backend    string   `json:"backend"`
	VCPUs      int      `json:"vcpus"`
	Mem        string   `json:"mem"`
	VSockPorts []uint32 `json:"vsock_ports"`
	Mounts     []struct {
		HostPath  string `json:"host_path"`
		GuestPath string `json:"guest_path"`
		Mode      string `json:"mode"`
	} `json:"mounts"`
}

func (d *daemon) sandboxCreate(params json.RawMessage) (any, error) {
	var p createParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse params: %w", err)
	}

	if d.backend == nil {
		return nil, fmt.Errorf("no VM backend available")
	}

	memMB := 8192
	if p.Mem != "" {
		fmt.Sscanf(p.Mem, "%dG", &memMB)
		memMB *= 1024
	}
	vcpus := p.VCPUs
	if vcpus == 0 {
		vcpus = 4
	}

	cfg := vm.Config{
		Backend:    p.Backend,
		VCPUs:      vcpus,
		MemoryMB:   memMB,
		VSockPorts: p.VSockPorts,
	}

	for _, m := range p.Mounts {
		cfg.Mounts = append(cfg.Mounts, vm.SharedDir{
			Tag:      m.GuestPath,
			HostPath: m.HostPath,
		})
	}

	v, err := d.backend.Create(cfg)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	d.currentVM = v

	return map[string]any{"id": v.ID()}, nil
}

func (d *daemon) sandboxStart() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM created")
	}

	if err := d.currentVM.Start(); err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}

	// Listen for vm-agent control channel
	controlListener, err := d.currentVM.VSockListen(1000)
	if err != nil {
		return nil, fmt.Errorf("vsock listen control: %w", err)
	}

	// Listen for data channel
	dataListener, err := d.currentVM.VSockListen(1001)
	if err != nil {
		controlListener.Close()
		return nil, fmt.Errorf("vsock listen data: %w", err)
	}

	log.Println("waiting for vm-agent control connection (timeout 30s)...")

	// Use a goroutine + channel to implement accept with timeout
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	controlCh := make(chan acceptResult, 1)
	go func() {
		conn, err := controlListener.Accept()
		controlCh <- acceptResult{conn, err}
	}()

	select {
	case result := <-controlCh:
		if result.err != nil {
			return nil, fmt.Errorf("accept control: %w", result.err)
		}
		log.Println("vm-agent control connected")

		log.Println("waiting for vm-agent data connection...")
		dataConn, err := dataListener.Accept()
		if err != nil {
			result.conn.Close()
			return nil, fmt.Errorf("accept data: %w", err)
		}
		log.Println("vm-agent data connected")
		d.setupAgent(result.conn, dataConn)

	case <-time.After(60 * time.Second):
		controlListener.Close()
		dataListener.Close()
		return nil, fmt.Errorf("timeout waiting for vm-agent to connect — VM may have failed to boot. Check if hv_sock module is available in the guest kernel")
	}

	return map[string]any{"ok": true}, nil
}

func (d *daemon) setupAgent(controlConn, dataConn net.Conn) {
	d.agentConn = rpc.NewConn(controlConn, controlConn)
	go d.agentConn.ReadLoop()

	// Forward vm-agent notifications to SDK
	go func() {
		for notif := range d.agentConn.Notifications {
			d.sdk.ForwardNotification(notif)
		}
	}()

	// Start netstack with the data channel
	var err error
	d.netSvc, err = netstack.New(dataConn)
	if err != nil {
		log.Printf("netstack setup failed: %v", err)
		return
	}
	go d.netSvc.Run()
	log.Println("netstack running")
}

func (d *daemon) sandboxStop() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM running")
	}
	if err := d.currentVM.Stop(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (d *daemon) sandboxDestroy() (any, error) {
	if d.currentVM == nil {
		return nil, fmt.Errorf("no VM to destroy")
	}
	if err := d.currentVM.Destroy(); err != nil {
		return nil, err
	}
	d.currentVM = nil
	d.agentConn = nil
	return map[string]any{"ok": true}, nil
}
