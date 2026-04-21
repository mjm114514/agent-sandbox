# Agent Sandbox Design Document

## Overview

Agent Sandbox provides lightweight, isolated execution environments for AI agents on local machines. It runs a single VM (Hyper-V on Windows, Apple Virtualization Framework on macOS) with a userspace networking stack on the host side. All network traffic originating from the VM is terminated and re-originated by the host-side process, making it indistinguishable from regular host process traffic to the rest of the network. This is particularly valuable in enterprise environments where enrolling a VM into corporate network infrastructure (certificates, proxies, MDM) would otherwise be required.

Inside the VM, multiple **environments** — each a bwrap-isolated, ephemeral user — give agents cheap, disposable workspaces without the cost of spinning up additional VMs.

### Design Principles

- **One VM per application.** The VM is a heavy, long-lived resource. The SDK creates it explicitly; there is no pool.
- **Environments are cheap.** Creating an environment is `useradd` + `bwrap` + optional cgroup limits. Agents interact with environments, not VMs.
- **Transparent host networking.** All guest traffic exits through a TAP-over-vsock tunnel to a gVisor netstack on the host. The host re-originates every connection as a regular syscall, so it inherits the host's DNS, proxy settings, TLS trust store, and network identity. The guest needs no network configuration of its own.
- **Composable primitives.** The SDK provides three building blocks for extensibility: **mounts** (share host files into the VM), **VM-level exec** (run services outside any environment), and **vsock ports** (host↔guest communication channels). Users combine these to build higher-level capabilities — custom proxies, credential injection, logging agents — without the sandbox needing to know about any of them.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│ Host                                                         │
│                                                              │
│  SDK (Python) ──stdio JSON-RPC──► as-hostd (Go)              │
│                                       │                      │
│                                       ├─ vm-manager          │
│                                       ├─ netstack-svc        │
│                                       └─ mount-broker        │
│                                       │                      │
│                          vsock ◄──────┘                      │
└───────────────────────────┼──────────────────────────────────┘
                            │
┌───────────────────────────┼──────────────────────────────────┐
│ VM (guest)                │                                  │
│                           ▼                                  │
│   as-guestd (vsock client → host)                            │
│     ├─ tap-bridge       TAP ↔ vsock tunnel                   │
│     ├─ env-manager      useradd / bwrap / cgroup             │
│     ├─ exec-runner      spawn & stream processes             │
│     └─ mount-mounter    bind virtiofs shares into bwrap      │
│                                                              │
│   [user-defined services via mount + exec + vsock]           │
└──────────────────────────────────────────────────────────────┘
```

### Components

#### as-hostd (Go, runs on host)

Single binary, spawned by the Python SDK as a child process. Communicates with the SDK over **stdin/stdout JSON-RPC 2.0**. Communicates with the guest as-guestd over **vsock JSON-RPC** (control plane) and a separate **vsock raw channel** (data plane for TAP frames).

Sub-modules:

| Module | Responsibility |
|---|---|
| **vm-manager** | Create / start / stop / destroy the VM. On Windows, calls the HCS (Host Compute Service) API directly via `computecore.dll` to create a LCOW utility VM with direct kernel boot. On macOS, uses Apple Virtualization.framework. Manages the VM lifecycle and vsock connections. |
| **netstack-svc** | Receives raw Ethernet frames from the guest TAP tunnel over vsock. Runs a full userspace TCP/IP stack using gVisor's `tcpip` and `stack` packages. For each outbound connection, performs a regular host-side `connect()` syscall, so the traffic inherits the host's network identity, DNS resolution, proxy settings, and TLS trust store. |
| **mount-broker** | Exposes host directories into the guest via virtiofs (preferred) or 9p. Tracks which host paths are shared and their corresponding guest mount points, so that `Environment` and `Sandbox`-level mount requests can reference them. |

#### as-guestd (Go, runs in guest)

Statically compiled binary, baked into the base VM image. Started by OpenRC on boot. Connects to the host as-hostd over vsock.

Sub-modules:

| Module | Responsibility |
|---|---|
| **tap-bridge** | Creates a TAP device, sets it as the guest's default route, and tunnels all frames over a dedicated vsock connection to the host netstack-svc. The guest kernel sees a normal network interface; no special configuration required. |
| **env-manager** | Handles `environment.create` RPCs: creates a temporary user, sets up a bwrap invocation with mount/pid/ipc namespace isolation, applies optional cgroup limits (cpu, memory). On `environment.close`: kills all processes, unmounts, deletes the user, cleans temp files. |
| **exec-runner** | Handles `exec` RPCs: spawns processes at either the VM level (as `sandbox-admin`) or inside a given environment's bwrap namespace (as that environment's user). Streams stdout/stderr back over the control vsock connection. Supports stdin forwarding, signals, and timeout. |
| **mount-mounter** | Mounts virtiofs/9p shares (exposed by the host mount-broker) at staging paths inside the VM, then binds them into the bwrap mount namespace for the target environment. For VM-level mounts, places them directly at the requested guest path. |

## Data Paths

### Control Plane

```
SDK (Python)
  ──stdio JSON-RPC──►
    as-hostd (Go)
      ──vsock JSON-RPC──►
        as-guestd (Go)
```

All SDK operations (`Sandbox.start`, `sb.exec`, `env.exec`, `env.close`, etc.) are JSON-RPC calls over this path. as-hostd translates SDK-level calls into as-guestd RPCs where needed, and handles host-local operations (VM lifecycle, netstack) directly.

### Network Data Plane

```
Guest process
  → kernel → TAP device
    → as-guestd tap-bridge
      → vsock (raw Ethernet frames, length-prefixed)
        → as-hostd netstack-svc
          → host-side connect() / sendto()
            → internet (as host process traffic)
```

Frame format on the vsock data channel: `[4-byte big-endian length][Ethernet frame]`.

Guest DNS queries are intercepted by netstack-svc and resolved using the host's DNS resolver, ensuring the guest sees the same DNS results as any host process.

## SDK Surface

The Python SDK is async-first. A synchronous wrapper is auto-generated.

### Object Model

```
Sandbox           VM lifecycle, VM-level exec & mounts, network, vsock
 ├─ Network       port forwarding, expose
 └─ Environment   bwrap-isolated user workspace (1:N per Sandbox)
      └─ Process  a running command inside an Environment
```

### API

```python
# --- Sandbox ---

class Sandbox:
    @classmethod
    async def create(
        cls,
        *,
        backend: Literal["hyperv", "avf", "auto"] = "auto",
        vcpus: int = 4,
        mem: str = "8G",
        mounts: list[Mount] = [],         # VM-level mounts, available to all environments
        vsock_ports: list[int] = [],      # additional vsock ports for user services
    ) -> Sandbox: ...

    async def start(self) -> None: ...
    async def stop(self) -> None: ...
    async def destroy(self) -> None: ...

    async def exec(
        self,
        argv: list[str],
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> Process: ...

    async def environment(
        self,
        name: str,
        *,
        mounts: list[Mount] = [],
        env: dict[str, str] = {},
        cwd: str = "/",
        cpu_limit: str | None = None,     # e.g. "2" (cores)
        mem_limit: str | None = None,     # e.g. "4G"
    ) -> Environment: ...

    @property
    def network(self) -> Network: ...

    async def vsock_connect(self, port: int) -> VsockStream: ...


# --- Environment ---

class Environment:
    name: str
    sandbox: Sandbox

    async def exec(
        self,
        argv: list[str],
        *,
        stdin: bytes | AsyncIterable[bytes] | None = None,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        stream: bool = False,
        timeout: float | None = None,
    ) -> Process: ...

    async def export(self, guest_path: str, host_path: str) -> None: ...
    async def close(self) -> None: ...

    async def __aenter__(self) -> Environment: ...
    async def __aexit__(self, *exc) -> None: ...


# --- Process ---

class Process:
    pid: int
    stdout: AsyncIterable[bytes]
    stderr: AsyncIterable[bytes]

    async def write(self, data: bytes) -> None: ...
    async def wait(self) -> int: ...
    async def kill(self, signal: int = 15) -> None: ...


# --- Networking ---

class Network:
    async def forward(self, host_port: int, guest_port: int) -> None: ...
    async def expose(self, guest_port: int) -> str: ...


# --- Vsock ---

class VsockStream:
    async def read(self, n: int) -> bytes: ...
    async def write(self, data: bytes) -> None: ...
    async def close(self) -> None: ...
    async def __aenter__(self) -> VsockStream: ...
    async def __aexit__(self, *exc) -> None: ...


# --- Mounts ---

class Mount:
    host_path: str
    guest_path: str
    mode: Literal["ro", "rw"] = "rw"
```

### Usage Example

```python
import asyncio
from sandbox import Sandbox, Mount

async def main():
    sb = await Sandbox.create(
        vcpus=4,
        mem="8G",
        mounts=[Mount("/path/to/my-tools", "/opt/tools", mode="ro")],
        vsock_ports=[2000],
    )
    await sb.start()

    # Start a custom service at the VM level (outside any environment)
    proxy = await sb.exec(["/opt/tools/my-proxy", "--vsock-port=2000"])

    # Create an environment for the agent
    async with await sb.environment(
        name="task-123",
        mounts=[Mount("D:/sources/myproject", "/work")],
        cwd="/work",
    ) as env:
        proc = await env.exec(["git", "log", "--oneline", "-10"], stream=True)
        async for chunk in proc.stdout:
            print(chunk.decode(), end="")
        code = await proc.wait()
        print(f"exit code: {code}")

    await proxy.kill()
    await sb.destroy()

asyncio.run(main())
```

## VM Image

The base image is a minimal Linux (e.g., Alpine or Ubuntu minimal) shipping with:

- **as-guestd** binary at `/usr/local/bin/as-guestd`, started by an OpenRC service.
- **bubblewrap** (`bwrap`) installed.
- **virtiofs** kernel support enabled.
- A passwordless `sandbox-admin` user for as-guestd to run as, with sudo rights for `useradd`, `userdel`, `bwrap`, and `cgcreate`.
- No SSH, no unnecessary services.

The image is intentionally minimal. Users extend the VM's capabilities at runtime by mounting host directories containing their binaries and configuration, then launching services via `Sandbox.exec()`. This avoids the need for custom image builds in most cases.

Image build tooling (Packer or a shell script) lives in `images/` in this repo.

## vsock Protocol

The system uses multiple vsock connections between as-hostd and as-guestd. Port 1000 and 1001 are reserved for the control and data channels. Additional ports can be requested by the user for custom services.

### Control Channel (CID=3, port 1000)

JSON-RPC 2.0 over a length-prefixed framing:

```
[4-byte big-endian payload length][JSON-RPC message (UTF-8)]
```

Methods exposed by as-guestd:

| Method | Params | Description |
|---|---|---|
| `env.create` | `{name, mounts, env, cwd, cpu_limit, mem_limit}` | Create environment |
| `env.close` | `{name}` | Tear down environment |
| `env.export` | `{name, guest_path, offset, chunk_size}` | Read a file chunk from the guest. Returns `{data_b64, total_size, offset, eof}` |
| `exec.start` | `{env?, argv, env_vars, cwd, timeout}` | Start a process. If `env` is omitted, runs at VM level as `sandbox-admin`. Returns `{pid}` |
| `exec.write` | `{pid, data_b64}` | Write to stdin |
| `exec.close_stdin` | `{pid}` | Close stdin |
| `exec.kill` | `{pid, signal}` | Send signal |
| `mount.bind` | `{virtiofs_tag, guest_path}` | Mount a virtiofs share |
| `mount.unbind` | `{guest_path}` | Unmount |
| `log.subscribe` | `{min_level}` | Enable log forwarding at or above `min_level` |

Notifications from as-guestd to as-hostd (JSON-RPC notifications, no response expected):

| Notification | Params | Description |
|---|---|---|
| `exec.stdout` | `{pid, data_b64}` | stdout chunk |
| `exec.stderr` | `{pid, data_b64}` | stderr chunk |
| `exec.exit` | `{pid, code}` | Process exited |
| `heartbeat.ping` | `{ts}` | Liveness ping (every 5s) |
| `log` | `{level, msg, ts}` | Structured log line (only after `log.subscribe`) |

### Data Channel (CID=3, port 1001)

Raw Ethernet frames, length-prefixed:

```
[4-byte big-endian frame length][Ethernet frame bytes]
```

No JSON-RPC on this channel. Bidirectional: host↔guest. This is the TAP tunnel.

### User-Defined Ports (port 2000+)

Ports requested via `vsock_ports` in `Sandbox.create()` are available for user-defined services. The SDK exposes them via `Sandbox.vsock_connect(port)`, which returns a bidirectional byte stream. The guest-side service listens on the corresponding vsock port directly.

## Platform Abstraction

### HCS (Windows)

- VM management: directly calls the Host Compute Service (HCS) API via `computecore.dll`. Creates a LCOW (Linux Containers on Windows) utility VM with direct kernel boot (vmlinuz + initramfs + rootfs VHDX).
- vsock: Hyper-V sockets (`hvsocket`) via `go-winio`. Uses `AF_HYPERV` with the VM's runtime GUID and a service GUID derived from the port number.
- File sharing: Plan 9 shares via HCS modify requests.

### AVF (macOS)

- VM management: Apple Virtualization.framework via cgo or a Swift helper.
- vsock: Standard virtio-vsock, `SOCK_STREAM` over `/dev/vsock`.
- virtiofs: Natively supported by AVF via `VZVirtioFileSystemDeviceConfiguration`.

The platform boundary is a Go interface:

```go
type VMBackend interface {
    Create(cfg VMConfig) (VM, error)
    Start(vm VM) error
    Stop(vm VM) error
    Destroy(vm VM) error
    VSockDial(vm VM, port uint32) (net.Conn, error)
    ShareDir(vm VM, tag string, hostPath string) error
}
```

## Security Considerations

- **Environments are not a security boundary.** They use Linux user isolation + bwrap namespaces. This prevents accidental cross-contamination between concurrent agent tasks but does not defend against a malicious guest process attempting privilege escalation. The VM boundary is the security boundary.
- **Network traffic is re-originated on the host.** The guest has no direct network access. All connections are made by the as-hostd process on behalf of the guest, inheriting the host's network identity and policies. This means host-level firewalls, proxies, and monitoring apply to guest traffic automatically.

## Repository Layout

```
agent-sandbox/
├── docs/
│   ├── design.md                # this document
│   └── error-recovery.md        # heartbeat, failure detection, logging
├── as-hostd/                    # Go module: host-side daemon
│   ├── cmd/as-hostd/            # main entry point
│   ├── vm/                      # VMBackend interface + implementations
│   │   └── hcs/                 # Windows HCS (macOS AVF: TODO)
│   ├── netstack/                # gVisor netstack integration
│   └── rpc/                     # JSON-RPC framing + stdio server
├── as-guestd/                   # Go module: guest-side daemon
│   ├── cmd/as-guestd/
│   ├── tap/                     # TAP device + vsock tunnel
│   ├── env/                     # useradd / bwrap / cgroup manager
│   ├── exec/                    # process runner, stdio streaming
│   ├── mount/                   # virtiofs / 9p bind helper
│   ├── log/                     # structured logger
│   └── rpc/                     # JSON-RPC framing
├── sdk/                         # Python SDK
│   └── sandbox/
│       ├── __init__.py
│       ├── sandbox.py           # Sandbox class
│       ├── environment.py       # Environment class
│       ├── process.py           # Process class
│       ├── network.py           # port forwarding / expose
│       ├── _rpc.py              # JSON-RPC over stdio
│       ├── _binary.py           # locate the as-hostd binary
│       ├── boot.py              # VM boot file pull/build/cache
│       ├── build.py             # build as-hostd from source
│       └── cli.py               # `sandbox` CLI entry point
├── images/                      # VM image build
│   ├── Dockerfile               # reproducible image build
│   ├── build-wsl.sh             # WSL-side direct build
│   ├── etc/                     # static rootfs config (fstab, ...)
│   └── openrc/                  # OpenRC init scripts
└── tests/
```
