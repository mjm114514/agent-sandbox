# 9P-over-TCP performance PoC

Measures 9P protocol + Linux kernel 9p-client overhead against the underlying
filesystem, to decide whether to invest in a self-hosted 9P server for
the File Guard feature (see `docs/file-guard.md`).

## What it measures

Four workloads relevant to agent I/O patterns:

| Workload | Why we care |
|---|---|
| Sequential read (100 MB) | Reading large artifacts, compiled binaries |
| Sequential write (100 MB) | Writing build output, logs |
| Stat 1000 small files | `git status`, dependency tree scans |
| Read 1000 small files | Compilation, config loading, source reading |

Both the 9P mount and the underlying native filesystem get the same workload.
The **ratio** (9P ÷ native) is the decision metric.

## What it does NOT measure

- **vsock transport**: uses TCP loopback instead. On a modern kernel the two
  are within ~10% for throughput and similar for per-op latency, so the ratio
  carries over. Running this inside the real VM (HCS / AVF) with vsock
  transport is a follow-up.
- **Our own 9P server**: uses `diod` (production C implementation). This
  isolates the protocol ceiling from library-choice variance. If 9P itself
  is fast enough, next step is comparing the Go library we'd actually ship
  (e.g. `hugelgupf/p9`).

## How to run

Inside WSL Ubuntu (or any Linux with 9p kernel module + sudo):

```bash
sudo apt-get install -y diod
sudo -v   # prime sudo
bash poc/9p-bench/bench.sh
```

Tunables via env:

```bash
BIG_FILE_MB=500 SMALL_FILE_COUNT=5000 bash poc/9p-bench/bench.sh
```

## Interpreting results

Expected rough shape:
- **Sequential read/write**: 9P usually reaches 50–80% of native on loopback.
  Agent workloads rarely saturate this — acceptable if ≥ ~200 MB/s.
- **Small-file ops**: this is where 9P hurts. Each op is a round-trip. Expect
  10–30% of native ops/s. If stat throughput drops below ~1000 ops/s, large
  source trees (`find`, `git status`, Python imports) will feel slow.

Decision heuristic:
- If small-file ops/s > 2000 → 9P is viable, proceed with server impl.
- If 500–2000 → usable for typical agent tasks, painful for monorepo work.
- If < 500 → reconsider: either accept as-is for security benefits, or switch
  to a FUSE-in-guest / hybrid approach with a fast-path for reads.
