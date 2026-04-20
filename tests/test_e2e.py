"""
End-to-end test: SDK → sandboxd → HCS → VM → vm-agent
Tests the full control plane and verifies vm-agent connectivity.
"""
import asyncio
import os
import struct
import json
import sys
import time

SANDBOXD = os.path.join(os.path.dirname(__file__), "..", "sandboxd", "sandboxd.exe")
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk"))


class SimpleRPC:
    """Minimal JSON-RPC client over length-prefixed stdio."""

    def __init__(self, reader, writer):
        self.reader = reader
        self.writer = writer
        self.next_id = 0

    async def call(self, method, params=None, timeout=30):
        self.next_id += 1
        msg = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params:
            msg["params"] = params

        body = json.dumps(msg).encode()
        self.writer.write(struct.pack(">I", len(body)) + body)
        await self.writer.drain()

        header = await asyncio.wait_for(self.reader.readexactly(4), timeout)
        length = struct.unpack(">I", header)[0]
        resp_bytes = await asyncio.wait_for(self.reader.readexactly(length), timeout)
        resp = json.loads(resp_bytes)

        if "error" in resp:
            raise Exception(f"RPC error: {resp['error']}")
        return resp.get("result")


async def main():
    print(f"sandboxd: {SANDBOXD}")
    print(f"boot dir: {os.path.join(os.path.dirname(SANDBOXD), 'boot')}")
    print()

    proc = await asyncio.create_subprocess_exec(
        SANDBOXD,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )

    # Start reading stderr in background for diagnostics
    async def read_stderr():
        while True:
            line = await proc.stderr.readline()
            if not line:
                break
            print(f"  [sandboxd] {line.decode().rstrip()}")

    stderr_task = asyncio.create_task(read_stderr())

    rpc = SimpleRPC(proc.stdout, proc.stdin)

    # --- Test 1: sandbox.create ---
    print("1. sandbox.create...")
    try:
        result = await rpc.call("sandbox.create", {
            "backend": "auto",
            "vcpus": 2,
            "mem": "2G",
        })
        print(f"   OK: {result}")
    except Exception as e:
        print(f"   FAIL: {e}")
        proc.kill()
        await proc.wait()
        return

    # --- Test 2: sandbox.start ---
    print("2. sandbox.start...")
    try:
        result = await rpc.call("sandbox.start", timeout=60)
        print(f"   OK: {result}")
    except Exception as e:
        print(f"   FAIL: {e}")
        # Try to clean up
        try:
            await rpc.call("sandbox.destroy", timeout=10)
        except:
            pass
        proc.kill()
        await proc.wait()
        return

    # Give vm-agent a moment to connect
    print("   Waiting for vm-agent...")
    await asyncio.sleep(5)

    # --- Test 3: exec at VM level ---
    print("3. exec (VM-level 'uname -a')...")
    try:
        result = await rpc.call("exec.start", {
            "argv": ["uname", "-a"],
            "timeout": 10,
        }, timeout=15)
        print(f"   OK: pid={result}")

        # Read stdout notification
        header = await asyncio.wait_for(proc.stdout.readexactly(4), 10)
        length = struct.unpack(">I", header)[0]
        notif = json.loads(await proc.stdout.readexactly(length))
        if notif.get("method") == "exec.stdout":
            import base64
            output = base64.b64decode(notif["params"]["data_b64"]).decode()
            print(f"   Output: {output.strip()}")
    except Exception as e:
        print(f"   FAIL: {e}")

    # --- Test 4: env.create ---
    print("4. env.create ('test-env')...")
    try:
        result = await rpc.call("env.create", {
            "name": "test-env",
            "cwd": "/tmp",
        }, timeout=15)
        print(f"   OK: {result}")
    except Exception as e:
        print(f"   FAIL: {e}")

    # --- Test 5: exec inside environment ---
    print("5. exec (env 'whoami')...")
    try:
        result = await rpc.call("exec.start", {
            "env": "test-env",
            "argv": ["whoami"],
        }, timeout=15)
        print(f"   OK: pid={result}")

        header = await asyncio.wait_for(proc.stdout.readexactly(4), 10)
        length = struct.unpack(">I", header)[0]
        notif = json.loads(await proc.stdout.readexactly(length))
        if notif.get("method") == "exec.stdout":
            import base64
            output = base64.b64decode(notif["params"]["data_b64"]).decode()
            print(f"   Output: {output.strip()}")
    except Exception as e:
        print(f"   FAIL: {e}")

    # --- Test 6: env.close ---
    print("6. env.close...")
    try:
        result = await rpc.call("env.close", {"name": "test-env"}, timeout=10)
        print(f"   OK: {result}")
    except Exception as e:
        print(f"   FAIL: {e}")

    # --- Test 7: sandbox.destroy ---
    print("7. sandbox.destroy...")
    try:
        result = await rpc.call("sandbox.destroy", timeout=15)
        print(f"   OK: {result}")
    except Exception as e:
        print(f"   FAIL: {e}")

    proc.kill()
    await proc.wait()
    stderr_task.cancel()

    print()
    print("Test complete.")


if __name__ == "__main__":
    asyncio.run(main())
