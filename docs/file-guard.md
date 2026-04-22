# File Guard

## Motivation

When a coding agent operates on the user's mounted directories, it will
sometimes do the wrong thing: overwrite a config, delete the wrong
file, corrupt a working tree. The sandbox's job is to make those
mistakes **visible** (show what changed) and **reversible** (get the
original back).

The hypervisor's native file-share paths (HCS Plan 9, AVF virtiofs) are
black boxes — the host has no protocol-level visibility into what the
guest does with shared directories. We replaced them with a
self-hosted 9P2000.L server running in-process with `as-hostd` on
vsock port 1002. Every SDK mount — sandbox-level and environment-level
— now rides this server. Without `file_guard=True` the server is pure
pass-through; with it enabled, every mutating op flows through a hook
that backs up the pre-mutation content and exposes a change list to
the SDK.

## Threat Model

Mistake containment, not adversarial hardening. The agent is benign
but fallible — wrong working directory, bad instruction, bug in its
own logic. It does not try to escape the sandbox.

Out of scope: adversarial agents, side channels, authenticated vsock
sessions, per-op access control. The trust boundary is the VM plus
bwrap; 9P-level identity is not authenticated. If the user doesn't
want the agent touching a tree, they don't mount it.

## Architecture

```
┌─ host (as-hostd) ──────────────────────────────────────────┐
│   SDK ─► sandbox.create(mounts=…)            ─► @vm mounts │
│   SDK ─► env.create(mounts, file_guard=True) ─► per-env    │
│                           │                                │
│                           ▼                                │
│                   9P server (hugelgupf/p9)                 │
│                     ├─ per-conn attacher (aname-routed)    │
│                     ├─ pass-through dispatch               │
│                     └─ FileGuard (Store attached per-env)  │
│                             ├─ touched map                 │
│                             └─ backup store                │
│                                   │                        │
│                                   ▼                        │
│                            vsock listener :1002            │
└────────────────────────────────────────────────────────────┘
                                    │
┌─ guest (as-guestd) ────────────────┼───────────────────────┐
│   file_share.mount ─► AF_VSOCK connect(host, 1002)         │
│                       write [2-byte len][aname] frame      │
│                       syscall.Mount(…, "9p", trans=fd,…)   │
└────────────────────────────────────────────────────────────┘
```

## Transport: vsock + trans=fd

The host 9P server listens on **vsock port 1002**.

In the guest, `as-guestd`:
1. Receives `file_share.mount {env_name, guest_path}` from `as-hostd`.
2. Opens a raw `AF_VSOCK` socket via direct syscall (bypassing Go's
   netpoller — keeps the fd simple and blocking).
3. Writes a length-prefixed aname frame to the socket (see **Aname
   routing** below).
4. Calls `mount(2)` directly via `syscall.Mount` with options
   `trans=fd,rfdno=N,wfdno=N,uname=root,aname=/,version=9p2000.L,msize=65536`
   where N is the socket fd.

Calling `mount(2)` directly instead of exec'ing mount(8) keeps the fd
in our own fd table, avoids `ExtraFiles` plumbing, and surfaces the
real errno on failure. The kernel 9p client does `fget(N)` on entry to
take its own reference, so closing our copy after `mount(2)` returns
doesn't tear down the session. The session dies when either end of
the vsock closes (`as-hostd` crash or explicit unmount).

`trans=fd` over vsock works identically on HCS (Hyper-V sockets) and
AVF (virtio-vsock). No dependency on hypervisor-provided 9P devices.

### Aname routing

`hugelgupf/p9` v0.3.0's `Attacher.Attach()` has no aname parameter,
and the Tattach message's `AttachName` is interpreted by the library
as a sub-path to walk into the root — unusable as a routing key.
Instead, the guest writes a length-prefixed frame at the start of
every connection, **before** the kernel 9P client takes over:

```
[2-byte big-endian length][aname UTF-8 bytes]
```

The host reads this frame synchronously, creates a per-connection
`p9.Server` whose `Attacher` has the aname baked in, then hands the
rest of the connection to `p9.Server.Handle`. The mount-option
`aname=/` asks the kernel to attach at the server's root.

**Aname format** is `<env-name>|<guest-path>`. For sandbox-level
mounts (those registered via `Sandbox.create(mounts=…)`) the
env-name is the reserved literal `@vm`. `@` can't appear in an
environment name because env names become Linux usernames prefixed
with `sandbox-`, so there's no collision.

## 9P Server

Built on `github.com/hugelgupf/p9` (9P2000.L). The server is a thin
translation over the host filesystem — `Twalk`/`Topen`/`Tread`/... map
directly to `openat`/`read`/... No caching, no policy. Without
FileGuard it's essentially `diod` in Go.

**QID identity** comes from `(st_dev, st_ino)` on Linux/macOS. On
Windows v1 uses a synthetic monotonic uint64 keyed by normalized
host path — a full FILE_ID_INFO lookup is planned but not required
for agent workloads in the benign-but-fallible threat model (a stale
QID after rename-over-unlink causes at worst a kernel 9p-client cache
miss, no correctness issue).

**Unsupported ops** return `EOPNOTSUPP`/`ENOSYS`: `Tmknod` (agents
don't need device nodes), `Tlock`/`Tgetlock` (silent success would
corrupt sqlite and git), `Txattrwalk`/`Txattrcreate` (rarely used;
reduces surface).

## File Guard

The 9P server is always the transport; File Guard is an opt-in hook
layered on top. Enable it via `file_guard=True` on `sb.environment()`.
When enabled, the mount registration carries a per-env `Store`, and
every mutating op checks in with the Store before the operation
proceeds: if this is the first mutation observed for the path, copy
current content to the backup dir, write a meta sidecar, add the
path to the `touched` map. Subsequent mutations on the same path are
pass-through.

No mount-time walk, no tracking of which files were there before, no
file-guard support at the sandbox (`@vm`) level in v1 — only named
environments can enable guard.

Agent-created files are treated the same way: the first time the
agent modifies one, the Store records an entry with
`backup_available=false` (there was nothing to copy). Slightly
redundant but keeps the rule uniform.

### SDK

```python
async with await sb.environment(
    "refactor-task",
    mounts=[Mount("/home/me/project", "/work")],
    file_guard=True,
) as env:
    proc = await env.exec(["my-agent", "refactor", "--aggressive"])
    await proc.wait()

    # Review what happened
    for entry in await env.file_guard.list():
        print(entry.current_state, entry.path)

    # Undo a specific change
    await env.file_guard.restore("/work/src/important.py")
```

### API

| Method | Behavior |
|---|---|
| `env.file_guard.list()` | Returns one entry per file whose first-mutation was observed. Current state is computed at call time by comparing the live filesystem to the backup. |
| `env.file_guard.restore(path)` | Writes the backup content to `path`. If a file currently exists at `path`, it's renamed to `<path>.as-preserved-<ts>` first. |
| `env.file_guard.status()` | `{enabled, touched_count, backup_bytes_used, backup_bytes_cap, backup_healthy}` |
| `env.file_guard.clear()` | Deletes all backups. Fresh start within the same env. |

`list()` entry shape:

```json
{
  "path": "/work/src/main.py",
  "first_mutation_ts": "2026-04-21T18:00:00Z",
  "size_at_backup": 1234,
  "current_state": "modified",
  "backup_available": true
}
```

`current_state`:

| Value | Meaning |
|---|---|
| `modified` | File exists at `path`, differs from the backup. |
| `deleted` | File does not exist at `path`. Includes rename-away. |
| `reverted` | File exists at `path` and byte-for-byte matches the backup. |

`file_guard=False` (default) registers the mount with a nil Store —
the 9P server pass-throughs every op without invoking any hook.

### Hook points

State: `touched map[string]*entry` keyed by canonical host path,
protected by a single `sync.Mutex`. One entry per distinct path
mutated this session.

Each op classifies which path(s) it mutates, then for each mutated
path: if not in `touched`, copy current content to `by-path/<rel>.orig`,
write `<rel>.meta.json`, add to `touched`. Otherwise pass through.

| 9P op | Mutates… |
|---|---|
| `Twrite` / `Topen(O_TRUNC)` / `Tsetattr` with size change | The fid's current path. |
| `Tunlinkat` | The deleted path. |
| `Trename` / `TrenameAt` | The source path; also the destination, only if it existed before the rename. |
| `Tcreate` | The created path (records it as `backup_available=false` — there was nothing to back up). |
| `Tsetattr` mode/uid/gid only | — (v1 doesn't back up metadata-only changes). |
| `Tread`, `Tgetattr`, `Treaddir` | — (pass-through). |

After a rename, subsequent mutations at the destination trigger a
fresh backup of the just-renamed-in content — often redundant with
the source's backup, but it keeps the model path-local and stateless.

### Backup storage

```
<state_dir>/guard/<env>/
├── manifest.json               # env name, start ts, cap
└── by-path/
    ├── work/src/main.py.orig
    ├── work/src/main.py.meta.json
    └── work/bar.txt.orig, .meta.json
```

`<state_dir>` is `$AS_HOSTD_STATE_DIR` if set, otherwise
`<UserCacheDir>/agent-sandbox` (`%LOCALAPPDATA%\agent-sandbox` on
Windows, `~/.cache/agent-sandbox` on Linux,
`~/Library/Caches/agent-sandbox` on macOS).

`by-path/` is the authoritative list of touched files — no separate
change log. On host paths, leading separator is stripped and any
Windows drive colon is flattened (`D:/foo/bar` → `D/foo/bar.orig`)
so the relative layout is valid on every OS. Sidecar meta:
`{first_mutation_ts, size_at_backup, backup_available, rel_path,
host_path, guest_path}`. Backups are plain file copies; no
compression in v1. The store is rebuilt from disk sidecars on
`Open`, so a crash mid-session leaves a recoverable state.

### Backup cap

Default 5 GiB per env. When a new backup would exceed the cap, the
mutation **proceeds** (blocking the agent doesn't help the user); we
write the `.meta.json` with `backup_available: false` and flip
`status().backup_healthy = false`. The file has no `.orig` and
`restore()` raises `NoBackupError` for it.

`clear()` is the user-facing release valve — drop all backups and
start over without tearing down the env.

### Restore

```python
await env.file_guard.restore("/work/src/main.py")
```

1. If there's no `.orig` (backup unavailable), raise `NoBackupError`.
2. If a file currently exists at `path`, rename it to
   `<path>.as-preserved-<ts>`.
3. Copy the backup content to `path`.
4. Delete the backup (`.orig` + `.meta.json`), so a later mutation of
   the same path triggers a fresh backup.

If the rename-target copy of a renamed-away file still exists
elsewhere, it's left alone — the user deletes it if they want.

## Failure and recovery

- **`as-hostd` crash**: backups survive on disk; the env is marked
  dead via the existing heartbeat mechanism (see `error-recovery.md`).
  Orphaned stores can be inspected directly at
  `<state_dir>/guard/<env>/` — the `by-path/` layout is stable and
  human-readable. We do not resume envs across restarts; the SDK
  reports a fresh guard view on each `env.create`.
- **Disk full on backup write**: same behavior as cap exceeded.

## Cross-platform

- **vsock**: Hyper-V sockets on Windows (via `go-winio`); virtio-vsock
  on macOS (via AVF's `VZVirtioSocketDevice`).
- **Symlinks**: since FileGuard keys by path, writes through a symlink
  hit the resolved path and are backed up there. Modifying a symlink
  itself is a mutation of the link path.

## Performance

- **Mount time**: O(1). No walk, no tree enumeration.
- **Per-op hot path**: one map lookup keyed by path. O(1). No
  measurable overhead on reads or on writes to already-backed-up
  files.
- **First mutation of a file**: one synchronous file copy (the
  backup). Proportional to file size. For trees dominated by large
  artifacts users shouldn't mount them — File Guard doesn't have a
  size-based opt-out in v1.
- **Memory**: one `map` entry per mutated file (~50 bytes). For a
  refactor touching 100 files, ~5 KB. No upfront allocation for the
  tree.
- **9P-over-vsock itself**: PoC benchmark (see `poc/9p-bench/`) on
  loopback TCP shows ~300 MB/s sequential throughput, ~110k stat/s,
  ~1.3k cold small-file reads/s. vsock is within ~10% of that.

## Non-goals

- **Versioning**: one backup per path; no multi-version history.
- **Access control**: File Guard doesn't deny any op; it tracks and
  preserves. Deny/read-only rules would be a separable feature on
  the same 9P server.
- **Protecting agent-created state**: creation isn't a mutation, so
  create-then-delete leaves no record.
- **Sandbox-level (`@vm`) file_guard**: only per-environment
  `file_guard=True` is wired. The plumbing to attach a Store at the
  `@vm` aname exists but isn't exposed through `Sandbox.create(…)`
  yet.

## Known limitations

1. **Agent-created file noise**: `list()` includes files the agent
   created and later modified. Usually small and harmless. If noisy
   in practice, a future version could observe `Tcreate` and suppress
   the first post-create entry.
2. **Large-file opt-out**: one giant `Twrite` on a 10 GiB file costs
   10 GiB of backup. A size threshold above which we record the
   mutation without an `.orig` (similar to the cap-exceeded case) is
   future work.
3. **Windows QID identity**: synthetic per-path uint64; a rename
   followed by a fresh file at the old path can reuse the QID and
   cause a kernel 9p-client cache miss. Not a correctness issue.
4. **FS-level locks**: `Tlock`/`Tgetlock` return `ENOSYS`. Tools that
   depend on advisory locking (sqlite under WAL, some git operations)
   may show warnings. The alternative — silent success — would
   corrupt data, so this stays opt-in at the tool level.
