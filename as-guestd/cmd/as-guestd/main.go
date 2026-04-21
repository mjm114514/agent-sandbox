package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/anthropics/agent-sandbox/as-guestd/env"
	vmexec "github.com/anthropics/agent-sandbox/as-guestd/exec"
	vmlog "github.com/anthropics/agent-sandbox/as-guestd/log"
	vmmount "github.com/anthropics/agent-sandbox/as-guestd/mount"
	"github.com/anthropics/agent-sandbox/as-guestd/rpc"
	"github.com/anthropics/agent-sandbox/as-guestd/tap"
)

const (
	hostCID     = 2
	controlPort = 1000
	dataPort    = 1001
)

var logger *vmlog.Logger

func main() {
	var err error
	logger, err = vmlog.New("/var/log/as-guestd.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Close()

	logger.Info("as-guestd starting")

	controlConn, err := vsock.Dial(hostCID, controlPort, nil)
	if err != nil {
		logger.Error("dial control vsock: %v", err)
		os.Exit(1)
	}
	defer controlConn.Close()
	logger.Info("control channel connected")

	dataConn, err := vsock.Dial(hostCID, dataPort, nil)
	if err != nil {
		logger.Error("dial data vsock: %v", err)
		os.Exit(1)
	}
	defer dataConn.Close()
	logger.Info("data channel connected")

	bridge, err := tap.New("tap0")
	if err != nil {
		logger.Error("create tap bridge: %v", err)
		os.Exit(1)
	}
	go func() {
		if err := bridge.RunTunnel(dataConn); err != nil {
			logger.Error("tap tunnel: %v", err)
		}
	}()
	logger.Info("tap bridge running")

	serve(controlConn)
}

func serve(conn net.Conn) {
	rc := rpc.NewConn(conn, conn)
	envMgr := env.NewManager()
	runner := vmexec.NewRunner(rc)
	mounter := vmmount.NewMounter()

	// Wire up log forwarding over RPC
	logger.SetNotifier(rc.Notify)

	// Start heartbeat
	go heartbeat(rc)

	for {
		msg, err := rc.Read()
		if err != nil {
			logger.Error("read rpc: %v", err)
			os.Exit(1)
		}
		if msg.ID == nil {
			// Handle incoming notifications (heartbeat.pong)
			if msg.Method == "heartbeat.pong" {
				continue
			}
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
			if rpcErr == nil {
				logger.Info("environment %q created", p.Name)
			} else {
				logger.Error("env.create %q: %v", p.Name, rpcErr)
			}
			result = map[string]any{"ok": true}

		case "env.close":
			var p struct{ Name string `json:"name"` }
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = envMgr.Close(p.Name)
			if rpcErr == nil {
				logger.Info("environment %q closed", p.Name)
			}
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

		case "exec.close_stdin":
			var p struct {
				PID int `json:"pid"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			rpcErr = runner.CloseStdin(p.PID)
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

		case "env.export":
			var p struct {
				Name      string `json:"name"`
				GuestPath string `json:"guest_path"`
				Offset    int64  `json:"offset"`
				ChunkSize int    `json:"chunk_size"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			chunkSize := p.ChunkSize
			if chunkSize <= 0 || chunkSize > 1024*1024 {
				chunkSize = 512 * 1024
			}
			result, rpcErr = readFileChunk(p.GuestPath, p.Offset, chunkSize)

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

		case "log.subscribe":
			var p struct {
				MinLevel string `json:"min_level"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				rpcErr = fmt.Errorf("parse params: %w", err)
				break
			}
			logger.Subscribe(vmlog.Level(p.MinLevel))
			logger.Info("log subscription set to %s", p.MinLevel)
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

func readFileChunk(path string, offset int64, chunkSize int) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	totalSize := info.Size()

	if offset >= totalSize {
		return map[string]any{
			"data_b64":   "",
			"total_size": totalSize,
			"offset":     offset,
			"eof":        true,
		}, nil
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	buf := make([]byte, chunkSize)
	n, err := f.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return nil, err
	}

	eof := offset+int64(n) >= totalSize
	return map[string]any{
		"data_b64":   base64.StdEncoding.EncodeToString(buf[:n]),
		"total_size": totalSize,
		"offset":     offset,
		"eof":        eof,
	}, nil
}

func heartbeat(rc *rpc.Conn) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		rc.Notify("heartbeat.ping", map[string]any{
			"ts": time.Now().Unix(),
		})
	}
}
