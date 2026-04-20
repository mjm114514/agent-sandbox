import asyncio
import json
import struct
from typing import Any


class RpcConn:
    def __init__(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
        self._reader = reader
        self._writer = writer
        self._next_id = 0
        self._pending: dict[int, asyncio.Future] = {}
        self._notifications: asyncio.Queue = asyncio.Queue()
        self._read_task: asyncio.Task | None = None

    def start(self):
        self._read_task = asyncio.create_task(self._read_loop())

    async def _read_loop(self):
        while True:
            header = await self._reader.readexactly(4)
            length = struct.unpack(">I", header)[0]
            body = await self._reader.readexactly(length)
            msg = json.loads(body)

            if "id" in msg and "method" not in msg:
                fut = self._pending.pop(msg["id"], None)
                if fut and not fut.done():
                    fut.set_result(msg)
            elif "method" in msg and "id" not in msg:
                await self._notifications.put(msg)

    async def call(self, method: str, params: Any = None) -> Any:
        self._next_id += 1
        msg_id = self._next_id

        msg = {"jsonrpc": "2.0", "id": msg_id, "method": method}
        if params is not None:
            msg["params"] = params

        fut: asyncio.Future = asyncio.get_event_loop().create_future()
        self._pending[msg_id] = fut

        body = json.dumps(msg).encode()
        self._writer.write(struct.pack(">I", len(body)) + body)
        await self._writer.drain()

        resp = await fut
        if "error" in resp:
            raise RpcError(resp["error"]["code"], resp["error"]["message"])
        return resp.get("result")

    async def close(self):
        if self._read_task:
            self._read_task.cancel()
        self._writer.close()


class RpcError(Exception):
    def __init__(self, code: int, message: str):
        self.code = code
        self.message = message
        super().__init__(f"RPC error {code}: {message}")
