package rpc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync"
	"testing"
)

func encodeFrame(msg *Message) []byte {
	msg.JSONRPC = "2.0"
	body, _ := json.Marshal(msg)
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	return buf
}

func decodeFrame(t *testing.T, r io.Reader) *Message {
	t.Helper()
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		t.Fatalf("read frame length: %v", err)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &msg
}

func TestCallAndResponse(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	conn := NewConn(clientR, clientW)
	go conn.ReadLoop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		msg := decodeFrame(t, serverR)
		if msg.Method != "test.echo" {
			t.Errorf("expected method test.echo, got %s", msg.Method)
		}
		id := *msg.ID
		resp := &Message{ID: &id, Result: json.RawMessage(`{"value":"hello"}`)}
		resp.JSONRPC = "2.0"
		body, _ := json.Marshal(resp)
		binary.Write(serverW, binary.BigEndian, uint32(len(body)))
		serverW.Write(body)
	}()

	result, err := conn.Call("test.echo", map[string]string{"input": "hello"})
	if err != nil {
		t.Fatalf("call error: %v", err)
	}
	wg.Wait()

	var res map[string]string
	json.Unmarshal(*result, &res)
	if res["value"] != "hello" {
		t.Errorf("expected hello, got %s", res["value"])
	}
}

func TestCallError(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	conn := NewConn(clientR, clientW)
	go conn.ReadLoop()

	go func() {
		msg := decodeFrame(t, serverR)
		id := *msg.ID
		resp := &Message{ID: &id, Error: &Error{Code: -32600, Message: "bad request"}}
		resp.JSONRPC = "2.0"
		body, _ := json.Marshal(resp)
		binary.Write(serverW, binary.BigEndian, uint32(len(body)))
		serverW.Write(body)
	}()

	_, err := conn.Call("bad.method", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	rpcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if rpcErr.Code != -32600 {
		t.Errorf("expected code -32600, got %d", rpcErr.Code)
	}
}

func TestNotifications(t *testing.T) {
	notif := &Message{Method: "event.fired", Params: json.RawMessage(`{"key":"val"}`)}
	frame := encodeFrame(notif)

	conn := NewConn(bytes.NewReader(frame), io.Discard)
	go conn.ReadLoop()

	msg := <-conn.Notifications
	if msg.Method != "event.fired" {
		t.Errorf("expected event.fired, got %s", msg.Method)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(nil, &buf)
	msg := &Message{Method: "ping"}
	conn.writeMessage(msg)

	decoded := decodeFrame(t, &buf)
	if decoded.Method != "ping" {
		t.Errorf("expected ping, got %s", decoded.Method)
	}
	if decoded.JSONRPC != "2.0" {
		t.Errorf("expected 2.0, got %s", decoded.JSONRPC)
	}
}
