package rpc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type Conn struct {
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
	nextID atomic.Uint64
}

func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: r, w: w}
}

func (c *Conn) Read() (*Message, error) {
	var length uint32
	if err := binary.Read(c.r, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read frame length: %w", err)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	var msg Message
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}
	return &msg, nil
}

func (c *Conn) Write(msg *Message) error {
	msg.JSONRPC = "2.0"
	buf, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := binary.Write(c.w, binary.BigEndian, uint32(len(buf))); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	if _, err := c.w.Write(buf); err != nil {
		return fmt.Errorf("write frame body: %w", err)
	}
	return nil
}

func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.Write(&Message{Method: method, Params: raw})
}

func (c *Conn) Reply(id uint64, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.Write(&Message{ID: &id, Result: raw})
}

func (c *Conn) ReplyError(id uint64, code int, message string) error {
	return c.Write(&Message{ID: &id, Error: &Error{Code: code, Message: message}})
}
