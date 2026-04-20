package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"

	"github.com/mdlayher/vsock"

	"github.com/anthropics/agent-sandbox/vm-agent/env"
	vmexec "github.com/anthropics/agent-sandbox/vm-agent/exec"
	vmmount "github.com/anthropics/agent-sandbox/vm-agent/mount"
	"github.com/anthropics/agent-sandbox/vm-agent/rpc"
	"github.com/anthropics/agent-sandbox/vm-agent/tap"
)

const (
	hostCID       = 2
	controlPort   = 1000
	dataPort      = 1001
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("vm-agent starting")

	controlConn, err := vsock.Dial(hostCID, controlPort, nil)
	if err != nil {
		log.Fatalf("dial control vsock: %v", err)
	}
	defer controlConn.Close()
	log.Println("control channel connected")

	dataConn, err := vsock.Dial(hostCID, dataPort, nil)
	if err != nil {
		log.Fatalf("dial data vsock: %v", err)
	}
	defer dataConn.Close()
	log.Println("data channel connected")

	bridge, err := tap.New("tap0")
	if err != nil {
		log.Fatalf("create tap bridge: %v", err)
	}
	go func() {
		if err := bridge.RunTunnel(dataConn); err != nil {
			log.Fatalf("tap tunnel: %v", err)
		}
	}()
	log.Println("tap bridge running")

	serve(controlConn)
}

func serve(conn net.Conn) {
	rc := rpc.NewConn(conn, conn)
	envMgr := env.NewManager()
	runner := vmexec.NewRunner(rc)
	mounter := vmmount.NewMounter()

	for {
		msg, err := rc.Read()
		if err != nil {
			log.Fatalf("read rpc: %v", err)
		}
		if msg.ID == nil {
			continue
		}
		id := *msg.ID

		var rpcErr error
		var result any

		switch msg.Method {
		case "env.create":
			var p env.CreateParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			_, rpcErr = envMgr.Create(p)
			result = map[string]any{"ok": true}

		case "env.close":
			var p struct{ Name string `json:"name"` }
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = envMgr.Close(p.Name)
			result = map[string]any{"ok": true}

		case "exec.start":
			var p vmexec.StartParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			result, rpcErr = runner.Start(p)

		case "exec.write":
			var p struct {
				PID     int    `json:"pid"`
				DataB64 string `json:"data_b64"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			data, err := base64.StdEncoding.DecodeString(p.DataB64)
			if err != nil {
				rpcErr = fmt.Errorf("decode base64: %w", err)
				break
			}
			rpcErr = runner.WriteStdin(p.PID, data)
			result = map[string]any{"ok": true}

		case "exec.kill":
			var p struct {
				PID    int `json:"pid"`
				Signal int `json:"signal"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = runner.Kill(p.PID, p.Signal)
			result = map[string]any{"ok": true}

		case "mount.bind":
			var p vmmount.BindParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = mounter.Bind(p)
			result = map[string]any{"ok": true}

		case "mount.unbind":
			var p struct{ GuestPath string `json:"guest_path"` }
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = mounter.Unbind(p.GuestPath)
			result = map[string]any{"ok": true}

		default:
			rpcErr = fmt.Errorf("unknown method: %s", msg.Method)
		}

		if rpcErr != nil {
			rc.ReplyError(id, -32603, rpcErr.Error())
		} else {
			rc.Reply(id, result)
		}
	}
}
