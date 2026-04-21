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

// Conn handles length-prefixed JSON-RPC 2.0 over a byte stream.
type Conn struct {
	r      io.Reader
	w      io.Writer
	mu     sync.Mutex
	nextID atomic.Uint64

	// pending tracks in-flight requests for Call().
	pendingMu sync.Mutex
	pending   map[uint64]chan *Message

	// Notifications received from the remote side.
	Notifications chan *Message
}

func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{
		r:             r,
		w:             w,
		pending:       make(map[uint64]chan *Message),
		Notifications: make(chan *Message, 256),
	}
}

func (c *Conn) readFrame() ([]byte, error) {
	var length uint32
	if err := binary.Read(c.r, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (c *Conn) writeFrame(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := binary.Write(c.w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := c.w.Write(data)
	return err
}

func (c *Conn) writeMessage(msg *Message) error {
	msg.JSONRPC = "2.0"
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.writeFrame(buf)
}

// ReadLoop reads messages and dispatches responses to pending Call()s,
// notifications to the Notifications channel. Run in a goroutine.
func (c *Conn) ReadLoop() error {
	for {
		frame, err := c.readFrame()
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		var msg Message
		if err := json.Unmarshal(frame, &msg); err != nil {
			continue
		}

		if msg.ID != nil && msg.Method == "" {
			// Response to a Call().
			c.pendingMu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.pendingMu.Unlock()
			if ok {
				ch <- &msg
			}
		} else if msg.Method != "" && msg.ID == nil {
			// Notification.
			select {
			case c.Notifications <- &msg:
			default:
			}
		}
	}
}

// Call sends a request and waits for the response.
func (c *Conn) Call(method string, params any) (*json.RawMessage, error) {
	id := c.nextID.Add(1)
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	ch := make(chan *Message, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeMessage(&Message{ID: &id, Method: method, Params: raw}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	resp := <-ch
	if resp.Error != nil {
		return nil, resp.Error
	}
	return &resp.Result, nil
}
