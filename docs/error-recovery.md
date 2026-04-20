# Error Recovery, Heartbeat, and Logging

## Overview

This document describes the mechanisms for detecting failures, maintaining connection health between sandboxd and vm-agent, and surfacing diagnostic information to SDK callers.

## Heartbeat

The control channel between sandboxd and vm-agent carries a bidirectional heartbeat to detect connection loss and VM hangs.

### Protocol

vm-agent sends a `heartbeat.ping` notification to sandboxd every **5 seconds**. sandboxd tracks the timestamp of the last received ping.

```json
{"jsonrpc": "2.0", "method": "heartbeat.ping", "params": {"ts": 1713600000}}
```

If sandboxd does not receive a ping for **15 seconds** (3 missed intervals), it considers the vm-agent unreachable and transitions the sandbox to a `degraded` state. The SDK is notified via a `sandbox.status` notification.

sandboxd also sends `heartbeat.pong` back to vm-agent so the guest can detect host-side failures:

```json
{"jsonrpc": "2.0", "method": "heartbeat.pong", "params": {"ts": 1713600000}}
```

If vm-agent does not receive a pong for 15 seconds, it logs a warning and continues operating — the guest has no alternative host to fall back to, but the log record helps diagnose connectivity issues.

### State Machine

```
                  ┌──────────┐
      start ─────►│  running  │◄──── heartbeat received
                  └─────┬────┘
                        │ 15s no heartbeat
                        ▼
                  ┌──────────┐
                  │ degraded  │◄──── heartbeat timeout
                  └─────┬────┘
                        │ heartbeat received
                        ├──────────────────► running (recovered)
                        │ 60s still degraded
                        ▼
                  ┌──────────┐
                  │   dead    │ ──── VM presumed crashed
                  └──────────┘
```

When the sandbox enters `dead` state, sandboxd:
1. Sends a `sandbox.status` notification to the SDK with `{"state": "dead", "reason": "..."}`.
2. Terminates the VM via HCS.
3. Rejects further RPC calls with a clear error message.

### SDK Surface

```python
sb.on_status(callback)    # register a status change handler
sb.status                 # "running" | "degraded" | "dead"
```

The SDK's `Sandbox.start()` begins the heartbeat monitor. `Sandbox.destroy()` stops it.

## Logging

### Guest-Side (vm-agent)

vm-agent writes structured log lines to `/var/log/vm-agent.log` in the guest. These logs are also forwarded to sandboxd over the control channel as notifications:

```json
{"jsonrpc": "2.0", "method": "log", "params": {"level": "error", "msg": "tap bridge failed", "ts": 1713600000}}
```

Log levels: `debug`, `info`, `warn`, `error`.

sandboxd forwards these to the SDK as `sandbox.log` notifications. The SDK provides a way to consume them:

```python
sb.on_log(callback)       # register a log handler
sb.on_log(lambda msg: print(f"[{msg['level']}] {msg['msg']}"))
```

By default, logs are not forwarded to avoid noise. The SDK enables forwarding by calling `log.subscribe` on the control channel:

```json
{"jsonrpc": "2.0", "id": 1, "method": "log.subscribe", "params": {"min_level": "info"}}
```

### Host-Side (sandboxd)

sandboxd writes its own logs to stderr (already the case). No changes needed — the SDK's subprocess already captures stderr.

### Log Export

The SDK provides a method to retrieve the full guest log file:

```python
logs = await sb.export_logs()   # returns the content of /var/log/vm-agent.log
```

This is implemented as a regular `exec` call: `cat /var/log/vm-agent.log`.

## Error Handling in RPC

### Timeout on Call

All SDK RPC calls have a configurable timeout (default 30s). If the call times out:
1. The SDK raises `RpcTimeoutError`.
2. The pending request is cleaned up.
3. The sandbox status is checked — if heartbeat is healthy, it's a slow operation; if not, the VM may be in trouble.

### Connection Loss

If the sandboxd process exits unexpectedly (stdin/stdout pipe breaks):
1. The SDK's `_read_loop` catches the `IncompleteReadError`.
2. All pending futures are rejected with `ConnectionLostError`.
3. `sb.status` transitions to `"dead"`.

### VM Crash

If the HCS VM exits unexpectedly (kernel panic, OOM kill):
1. The vsock connections close, triggering EOF in the heartbeat monitor.
2. sandboxd detects the heartbeat timeout and moves to `dead`.
3. sandboxd sends `sandbox.status` notification with the reason.
4. Subsequent RPC calls return clear errors.

## Implementation Plan

### vm-agent changes
- Add heartbeat goroutine: sends `heartbeat.ping` every 5s on the control channel.
- Add `log.subscribe` RPC handler.
- Add structured logging helper that writes to both file and control channel.

### sandboxd changes
- Add heartbeat monitor goroutine: tracks last ping time, manages state transitions.
- Forward `sandbox.status` and `sandbox.log` notifications to SDK.
- Add `sandbox.status` RPC method for polling.

### SDK changes
- Add `Sandbox.status` property and `on_status()` / `on_log()` callback registration.
- Add `RpcTimeoutError` with configurable timeout.
- Add `export_logs()` method.
- Handle connection loss gracefully in `_read_loop`.
