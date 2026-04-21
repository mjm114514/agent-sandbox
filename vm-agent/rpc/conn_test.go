package rpc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
)

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

func encodeFrame(msg *Message) []byte {
	msg.JSONRPC = "2.0"
	body, _ := json.Marshal(msg)
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	return buf
}

func TestReadMessage(t *testing.T) {
	id := uint64(1)
	msg := &Message{ID: &id, Method: "exec.start", Params: json.RawMessage(`{"argv":["/bin/sh"]}`)}
	frame := encodeFrame(msg)

	conn := NewConn(bytes.NewReader(frame), io.Discard)
	got, err := conn.Read()
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if got.Method != "exec.start" {
		t.Errorf("expected exec.start, got %s", got.Method)
	}
	if *got.ID != 1 {
		t.Errorf("expected id 1, got %d", *got.ID)
	}
}

func TestReply(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(nil, &buf)
	conn.Reply(42, map[string]any{"ok": true})

	got := decodeFrame(t, &buf)
	if *got.ID != 42 {
		t.Errorf("expected id 42, got %d", *got.ID)
	}
	var result map[string]bool
	json.Unmarshal(got.Result, &result)
	if !result["ok"] {
		t.Error("expected ok=true")
	}
}

func TestReplyError(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(nil, &buf)
	conn.ReplyError(7, -32603, "internal error")

	got := decodeFrame(t, &buf)
	if *got.ID != 7 {
		t.Errorf("expected id 7, got %d", *got.ID)
	}
	if got.Error == nil {
		t.Fatal("expected error")
	}
	if got.Error.Code != -32603 {
		t.Errorf("expected -32603, got %d", got.Error.Code)
	}
}

func TestNotify(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(nil, &buf)
	conn.Notify("heartbeat.ping", map[string]any{"ts": 12345})

	got := decodeFrame(t, &buf)
	if got.Method != "heartbeat.ping" {
		t.Errorf("expected heartbeat.ping, got %s", got.Method)
	}
	if got.ID != nil {
		t.Error("notification should not have id")
	}
}

func TestWriteConcurrency(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(nil, &buf)

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			conn.Notify("test", map[string]any{"i": i})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
