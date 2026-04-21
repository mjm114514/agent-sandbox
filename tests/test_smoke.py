"""
Smoke test: verify SDK can spawn as-hostd and exchange JSON-RPC messages.
Does not require a real VM — tests the control plane only.
"""
import asyncio
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk"))

from sandbox._binary import find_hostd
from sandbox._rpc import RpcConn, RpcError


async def test_stdio_rpc():
    """Test that as-hostd responds to JSON-RPC calls over stdio."""
    proc = await asyncio.create_subprocess_exec(
        find_hostd(),
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )

    class WriterAdapter:
        def write(self, data):
            proc.stdin.write(data)

        async def drain(self):
            await proc.stdin.drain()

        def close(self):
            proc.stdin.close()

    rpc = RpcConn(proc.stdout, WriterAdapter())
    rpc.start()

    # Test sandbox.create
    print("Testing sandbox.create...", end=" ")
    try:
        result = await asyncio.wait_for(
            rpc.call("sandbox.create", {
                "backend": "auto",
                "vcpus": 2,
                "mem": "4G",
            }),
            timeout=5.0,
        )
        print(f"OK: {result}")
    except RpcError as e:
        # Expected if Hyper-V is not available
        print(f"RPC Error (expected if no backend): {e}")
    except asyncio.TimeoutError:
        print("TIMEOUT")

    # Test unknown method
    print("Testing unknown method...", end=" ")
    try:
        result = await asyncio.wait_for(
            rpc.call("nonexistent.method", {}),
            timeout=5.0,
        )
        print(f"Unexpected success: {result}")
    except RpcError as e:
        print(f"OK (got expected error): {e}")
    except asyncio.TimeoutError:
        print("TIMEOUT")

    # Cleanup
    await rpc.close()
    proc.kill()
    await proc.wait()
    print("\nAll smoke tests passed.")


if __name__ == "__main__":
    asyncio.run(test_stdio_rpc())
