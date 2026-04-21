# File Sharing with Guard

## Motivation

When a coding agent operates on the user's mounted directories, it will
sometimes do the wrong thing: overwrite a config, delete the wrong
file, corrupt a working tree. The sandbox's job is to make those
mistakes **visible** (show what changed) and **reversible** (get the
original back).

Stock hypervisor shares (HCS Plan 9, AVF virtiofs) have no hook for
this. We replace them with a self-hosted 9P server running in-process
with `as-hostd`. The server is pass-through by default; a per-env
**File Guard** feature backs up any file on first modification and
exposes a change list to the SDK.

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
┌─ host (as-hostd) ──────────────────────────────────┐
│   SDK ─► env.create(mounts, file_guard=True)       │
│                           │                        │
│                           ▼                        │
│                   9P server (hugelgupf/p9)         │
│                     ├─ pass-through dispatch       │
│                     └─ FileGuard (per env, opt-in) │
│                             ├─ touched map         │
│                             └─ backup store        │
│                                   │                │
│                                   ▼                │
│                            vsock listener :1002    │
└────────────────────────────────────────────────────┘
                                    │
┌─ guest (as-guestd) ────────────────┼───────────────┐
│   file_share.mount ─► vsock.Dial(host, 1002)       │
│                       mount -t 9p -o trans=fd,...  │
└────────────────────────────────────────────────────┘
```

## Transport: vsock + trans=fd

The host 9P server listens on **vsock port 1002**.

In the guest, `as-guestd`:
1. Receives `file_share.mount {env_name, guest_path}` from `as-hostd`.
2. Dials `vsock.Dial(hostCID, 1002)` — full-duplex FD.
3. Issues `mount -t 9p -o trans=fd,rfdno=N,wfdno=N,uname=<env>,aname=<env>,version=9p2000.L,msize=1048576,cache=loose <target>`.

The kernel 9p client dups the FD during `mount(2)`. `as-guestd`'s
`mount` subprocess closes its copy after the syscall returns; the
kernel's dup keeps the session alive regardless of `as-guestd`
lifecycle. Session dies only when the **host** end of the vsock closes
(`as-hostd` crash or explicit unmount).

`trans=fd` over vsock works identically on HCS (Hyper-V sockets) and
AVF (virtio-vsock). No dependency on hypervisor-provided 9P devices.

## 9P Server

Built on `github.com/hugelgupf/p9` (9P2000.L). The server is a thin
translation over the host filesystem — `Twalk`/`Topen`/`Tread`/... map
directly to `openat`/`read`/... No caching, no policy. Without
FileGuard it's essentially `diod` in Go.

**QID identity** comes from `(st_dev, st_ino)` on Linux/macOS and
`FILE_ID_INFO` on Windows — required for kernel 9p-client cache
coherence on rename/hardlink. Windows 8+ only.

**Unsupported ops** return `EOPNOTSUPP`: `Tmknod` (agents don't need
device nodes), `Tlock`/`Tgetlock` (silent success would corrupt sqlite
and git), `Txattrwalk`/`Txattrcreate` (rarely used; reduces surface).

## File Guard

Opt-in per env via `file_guard=True`. When enabled, the server
intercepts the first mutating op on each path — copy current content
to backup, then let the op proceed. Subsequent mutations on the same
path pass through unchanged. No mount-time walk, no tracking of which
files were there before.

Agent-created files are treated the same way: the first time the
agent modifies one, we back up whatever's there. Slightly redundant
but keeps the rule uniform.

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
    for c in await env.file_guard.list():
        print(c["current_state"], c["path"])

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

`file_guard=False` (default) disables all hooks — zero overhead.

### Hook points

State: `touched map[string]struct{}` keyed by canonical host path,
protected by a per-path lock. One entry per distinct path mutated
this session.

Each op classifies which path(s) it mutates, then for each mutated
path: if not in `touched`, copy current content to `by-path/<rel>.orig`,
write `<rel>.meta.json`, add to `touched`. Otherwise pass through.

| 9P op | Mutates… |
|---|---|
| `Twrite` / `Topen(O_TRUNC)` / `Tsetattr` with size change | The fid's current path. |
| `Tunlinkat` | The deleted path. |
| `Trename` | The source path; also the destination, only if it existed before the rename. |
| `Tsetattr` mode/uid/gid only | — (v1 doesn't back up metadata-only changes). |
| `Tcreate`, `Tread`, `Tgetattr`, `Treaddir` | — (pass-through). |

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

`by-path/` is the authoritative list of touched files — no separate
change log. Sidecar meta: `{first_mutation_ts, size_at_backup}`.
Backups are plain file copies; no compression in v1.

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
  The SDK can inspect the orphaned store via
  `sb.file_guard.list_orphaned(env_name)` for post-mortem, then drop
  `<state_dir>/guard/<env>/`. We do not resume envs across restarts.
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

## Open questions

1. **Agent-created file noise**: `list()` includes files the agent
   created and later modified. Usually small and harmless. If noisy
   in practice, v2 could observe `Tcreate` and suppress the first
   post-create backup.
2. **Large-file opt-out**: one giant `Twrite` on a 10 GiB file costs
   10 GiB of backup. Worth a size threshold above which we record
   the mutation without an `.orig`, like the cap-exceeded case?
