"""
End-to-end test: SDK → sandboxd → HCS → VM → vm-agent
"""
import asyncio
import os
import struct
import json
import sys
import base64

SANDBOXD = os.path.join(os.path.dirname(__file__), "..", "sandboxd", "sandboxd.exe")
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk"))


class SimpleRPC:
    def __init__(self, reader, writer):
        self.reader = reader
        self.writer = writer
        self.next_id = 0
        self._pending = {}
        self._notifications = asyncio.Queue()
        self._read_task = None

    def start(self):
        self._read_task = asyncio.create_task(self._read_loop())

    async def _read_loop(self):
        try:
            while True:
                header = await self.reader.readexactly(4)
                length = struct.unpack(">I", header)[0]
                body = await self.reader.readexactly(length)
                msg = json.loads(body)

                if "id" in msg and "method" not in msg:
                    fut = self._pending.pop(msg["id"], None)
                    if fut and not fut.done():
                        fut.set_result(msg)
                elif "method" in msg and "id" not in msg:
                    await self._notifications.put(msg)
        except Exception:
            pass

    async def call(self, method, params=None, timeout=30):
        self.next_id += 1
        msg = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params:
            msg["params"] = params

        fut = asyncio.get_event_loop().create_future()
        self._pending[self.next_id] = fut

        body = json.dumps(msg).encode()
        self.writer.write(struct.pack(">I", len(body)) + body)
        await self.writer.drain()

        resp = await asyncio.wait_for(fut, timeout)
        if "error" in resp:
            raise Exception(f"RPC error: {resp['error']}")
        return resp.get("result")

    async def read_notification(self, timeout=10):
        return await asyncio.wait_for(self._notifications.get(), timeout)


async def main():
    print(f"sandboxd: {SANDBOXD}")
    print()

    proc = await asyncio.create_subprocess_exec(
        SANDBOXD,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )

    async def read_stderr():
        while True:
            line = await proc.stderr.readline()
            if not line:
                break
            print(f"  [sandboxd] {line.decode().rstrip()}")

    stderr_task = asyncio.create_task(read_stderr())
    rpc = SimpleRPC(proc.stdout, proc.stdin)
    rpc.start()

    passed = 0
    failed = 0

    async def test(name, coro):
        nonlocal passed, failed
        print(f"{name}...", end=" ", flush=True)
        try:
            result = await coro
            print(f"OK{f': {result}' if result else ''}")
            passed += 1
            return result
        except Exception as e:
            print(f"FAIL: {e}")
            failed += 1
            return None

    # 1. Create
    await test("1. sandbox.create", rpc.call("sandbox.create", {
        "backend": "auto", "vcpus": 2, "mem": "2G",
    }))

    # 2. Start
    result = await test("2. sandbox.start", rpc.call("sandbox.start", timeout=90))
    if result is None:
        proc.kill()
        await proc.wait()
        return

    await asyncio.sleep(2)

    # 3. VM-level exec
    print("3. exec (VM-level 'uname -a')...", end=" ", flush=True)
    try:
        result = await rpc.call("exec.start", {"argv": ["uname", "-a"]}, timeout=15)
        pid = result["pid"]
        # Collect stdout
        output = b""
        while True:
            notif = await rpc.read_notification(timeout=10)
            params = notif.get("params", {})
            if params.get("pid") != pid:
                continue
            if notif["method"] == "exec.stdout":
                output += base64.b64decode(params["data_b64"])
            elif notif["method"] == "exec.exit":
                break
        print(f"OK: {output.decode().strip()}")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # 4. Create environment
    await test("4. env.create", rpc.call("env.create", {
        "name": "test-env", "cwd": "/tmp",
    }))

    # 5. Exec in environment
    print("5. exec (env 'whoami')...", end=" ", flush=True)
    try:
        result = await rpc.call("exec.start", {
            "env": "test-env", "argv": ["whoami"],
        }, timeout=15)
        pid = result["pid"]
        output = b""
        while True:
            notif = await rpc.read_notification(timeout=10)
            params = notif.get("params", {})
            if params.get("pid") != pid:
                continue
            if notif["method"] == "exec.stdout":
                output += base64.b64decode(params["data_b64"])
            elif notif["method"] == "exec.exit":
                break
        print(f"OK: {output.decode().strip()}")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # 6. Close environment
    await test("6. env.close", rpc.call("env.close", {"name": "test-env"}))

    # 7. Destroy
    await test("7. sandbox.destroy", rpc.call("sandbox.destroy", timeout=15))

    proc.kill()
    await proc.wait()
    stderr_task.cancel()

    print(f"\nResults: {passed} passed, {failed} failed")


if __name__ == "__main__":
    asyncio.run(main())
