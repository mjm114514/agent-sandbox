import asyncio
import json
import struct
import pytest
from sandbox._rpc import RpcConn, RpcError, RpcTimeoutError


def encode_frame(msg: dict) -> bytes:
    body = json.dumps(msg).encode()
    return struct.pack(">I", len(body)) + body


class MockReader:
    def __init__(self, data: bytes = b""):
        self._buf = asyncio.StreamReader()
        self._buf.feed_data(data)

    def feed(self, data: bytes):
        self._buf.feed_data(data)

    def feed_eof(self):
        self._buf.feed_eof()

    async def readexactly(self, n):
        return await self._buf.readexactly(n)


class MockWriter:
    def __init__(self):
        self.data = bytearray()
        self._closed = False

    def write(self, data):
        self.data.extend(data)

    async def drain(self):
        pass

    def close(self):
        self._closed = True


@pytest.mark.asyncio
async def test_call_and_response():
    reader = MockReader()
    writer = MockWriter()
    conn = RpcConn(reader, writer)
    conn.start()

    async def respond():
        await asyncio.sleep(0.01)
        # Parse the request from writer to get the id
        length = struct.unpack(">I", bytes(writer.data[:4]))[0]
        req = json.loads(writer.data[4:4 + length])
        resp = {"jsonrpc": "2.0", "id": req["id"], "result": {"value": 42}}
        reader.feed(encode_frame(resp))

    task = asyncio.create_task(respond())
    result = await conn.call("test.method", {"key": "val"})
    await task
    assert result == {"value": 42}
    await conn.close()


@pytest.mark.asyncio
async def test_call_error():
    reader = MockReader()
    writer = MockWriter()
    conn = RpcConn(reader, writer)
    conn.start()

    async def respond():
        await asyncio.sleep(0.01)
        length = struct.unpack(">I", bytes(writer.data[:4]))[0]
        req = json.loads(writer.data[4:4 + length])
        resp = {"jsonrpc": "2.0", "id": req["id"], "error": {"code": -32600, "message": "bad"}}
        reader.feed(encode_frame(resp))

    task = asyncio.create_task(respond())
    with pytest.raises(RpcError) as exc_info:
        await conn.call("bad.method")
    await task
    assert exc_info.value.code == -32600
    await conn.close()


@pytest.mark.asyncio
async def test_call_timeout():
    reader = MockReader()
    writer = MockWriter()
    conn = RpcConn(reader, writer)
    conn.start()

    with pytest.raises(RpcTimeoutError):
        await conn.call("slow.method", timeout=0.05)
    await conn.close()


@pytest.mark.asyncio
async def test_notification():
    reader = MockReader()
    writer = MockWriter()
    conn = RpcConn(reader, writer)

    received = []
    conn.on_notification("event", lambda msg: received.append(msg))
    conn.start()

    notif = {"jsonrpc": "2.0", "method": "event", "params": {"data": "hello"}}
    reader.feed(encode_frame(notif))
    await asyncio.sleep(0.05)

    assert len(received) == 1
    assert received[0]["params"]["data"] == "hello"
    await conn.close()


@pytest.mark.asyncio
async def test_off_notification():
    reader = MockReader()
    writer = MockWriter()
    conn = RpcConn(reader, writer)

    received = []
    handler = lambda msg: received.append(msg)
    conn.on_notification("event", handler)
    conn.off_notification("event", handler)
    conn.start()

    notif = {"jsonrpc": "2.0", "method": "event", "params": {}}
    reader.feed(encode_frame(notif))
    await asyncio.sleep(0.05)

    assert len(received) == 0
    await conn.close()


@pytest.mark.asyncio
async def test_frame_format():
    writer = MockWriter()
    reader = MockReader()
    conn = RpcConn(reader, writer)
    conn.start()

    # Don't await the call — just check the written frame
    asyncio.create_task(conn.call("test", {"a": 1}, timeout=0.05))
    await asyncio.sleep(0.01)

    length = struct.unpack(">I", bytes(writer.data[:4]))[0]
    msg = json.loads(writer.data[4:4 + length])
    assert msg["jsonrpc"] == "2.0"
    assert msg["method"] == "test"
    assert msg["params"] == {"a": 1}
    assert "id" in msg
    await conn.close()
