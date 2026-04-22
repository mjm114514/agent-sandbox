"""
End-to-end test for the File Guard flow:

  host temp dir (a.txt, b.txt, c.txt)
    → Sandbox with Mount(file_guard=True) at /work
    → guest command mutates a.txt, deletes b.txt, leaves c.txt
    → env.file_guard.list() reports modified + deleted
    → env.file_guard.restore(...) puts originals back on the host

Requires a built VM image (rootfs.vhdx + as-guestpack.vhdx). See README.md.

Run:
  cd sdk && pip install -e .
  python tests/test_file_guard_e2e.py
"""
from __future__ import annotations

import asyncio
import os
import shutil
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk"))

from sandbox import Sandbox, Mount, NoBackupError


ORIGINALS = {
    "a.txt": b"ORIGINAL A\n",
    "b.txt": b"ORIGINAL B\n",
    "c.txt": b"ORIGINAL C\n",
}


def seed(host_dir: str) -> None:
    for name, content in ORIGINALS.items():
        with open(os.path.join(host_dir, name), "wb") as f:
            f.write(content)


def check(cond: bool, msg: str) -> None:
    if not cond:
        print(f"  FAIL: {msg}")
        raise SystemExit(1)
    print(f"  OK:   {msg}")


async def main() -> None:
    host_dir = tempfile.mkdtemp(prefix="as-fg-e2e-")
    try:
        seed(host_dir)
        print(f"host dir: {host_dir}")
        for name in ORIGINALS:
            print(f"  seeded {name}: {ORIGINALS[name]!r}")

        print("\n== boot sandbox ==")
        sb = await Sandbox.create(vcpus=2, mem="2G")
        await sb.start()

        try:
            print("\n== create env with file_guard=True ==")
            async with await sb.environment(
                name="fg-test",
                mounts=[Mount(host_dir, "/work", mode="rw")],
                cwd="/work",
                file_guard=True,
            ) as env:

                # The 9P client caches aggressively; drop caches to make
                # the assertion below deterministic across kernels.
                await run(env, ["sh", "-c", "ls /work"])

                print("\n== agent mutates files ==")
                # Overwrite a.txt (write), delete b.txt (unlink), leave c.txt.
                rc, out = await run(env, [
                    "sh", "-c",
                    "echo AGENT_MODIFIED > /work/a.txt && rm /work/b.txt && echo done",
                ])
                check(rc == 0, f"agent command exited 0 (got {rc}, stdout={out!r})")

                print("\n== env.file_guard.list() ==")
                entries = await env.file_guard.list()
                by_path = {e.path: e for e in entries}
                print(f"  entries: {list(by_path.keys())}")
                check("/work/a.txt" in by_path, "a.txt is tracked")
                check("/work/b.txt" in by_path, "b.txt is tracked")
                check("/work/c.txt" not in by_path, "c.txt is NOT tracked (never touched)")
                check(by_path["/work/a.txt"].current_state == "modified",
                      f"a.txt state is modified (got {by_path['/work/a.txt'].current_state})")
                check(by_path["/work/b.txt"].current_state == "deleted",
                      f"b.txt state is deleted (got {by_path['/work/b.txt'].current_state})")
                check(by_path["/work/a.txt"].backup_available,
                      "a.txt has a backup")
                check(by_path["/work/b.txt"].backup_available,
                      "b.txt has a backup")

                print("\n== env.file_guard.status() ==")
                st = await env.file_guard.status()
                print(f"  status: enabled={st.enabled} touched={st.touched_count} "
                      f"used={st.backup_bytes_used}B healthy={st.backup_healthy}")
                check(st.enabled, "guard is enabled")
                check(st.touched_count == 2, f"touched_count=2 (got {st.touched_count})")
                check(st.backup_healthy, "guard is healthy")

                print("\n== restore a.txt ==")
                await env.file_guard.restore("/work/a.txt")
                with open(os.path.join(host_dir, "a.txt"), "rb") as f:
                    got = f.read()
                check(got == ORIGINALS["a.txt"],
                      f"a.txt restored on host (got {got!r})")
                # The .as-preserved-<ts> file should sit next to it.
                preserved = [n for n in os.listdir(host_dir)
                             if n.startswith("a.txt.as-preserved-")]
                check(len(preserved) == 1,
                      f"one .as-preserved-<ts> file next to a.txt (got {preserved})")

                print("\n== restore b.txt (was deleted) ==")
                await env.file_guard.restore("/work/b.txt")
                with open(os.path.join(host_dir, "b.txt"), "rb") as f:
                    got = f.read()
                check(got == ORIGINALS["b.txt"],
                      f"b.txt restored after delete (got {got!r})")

                print("\n== restore with no backup raises NoBackupError ==")
                try:
                    await env.file_guard.restore("/work/c.txt")
                    print("  FAIL: expected NoBackupError")
                    raise SystemExit(1)
                except NoBackupError as e:
                    print(f"  OK:   NoBackupError raised: {e}")

                print("\n== clear() resets the store ==")
                await env.file_guard.clear()
                st = await env.file_guard.status()
                check(st.touched_count == 0,
                      f"after clear touched_count=0 (got {st.touched_count})")

        finally:
            print("\n== destroy sandbox ==")
            await sb.destroy()

    finally:
        shutil.rmtree(host_dir, ignore_errors=True)

    print("\nALL PASS")


async def run(env, argv: list[str]) -> tuple[int, bytes]:
    proc = await env.exec(argv, stream=True)
    out = b""
    async for chunk in proc.stdout_stream():
        out += chunk
    rc = await proc.wait()
    return rc, out


if __name__ == "__main__":
    asyncio.run(main())
