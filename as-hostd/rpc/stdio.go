package rpc

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
)

// StdioServer reads JSON-RPC messages from the SDK (over stdin) and writes
// responses to stdout. It forwards calls to the as-guestd Conn when needed.
type StdioServer struct {
	conn    *Conn       // connection to the SDK (stdin/stdout)
	agent   *Conn       // connection to the as-guestd (vsock control)
	handler func(method string, params json.RawMessage) (any, error)
}

func NewStdioServer(stdin io.Reader, stdout io.Writer, agent *Conn) *StdioServer {
	return &StdioServer{
		conn:  NewConn(stdin, stdout),
		agent: agent,
	}
}

func (s *StdioServer) SetHandler(h func(method string, params json.RawMessage) (any, error)) {
	s.handler = h
}

func (s *StdioServer) Serve() error {
	for {
		frame, err := s.conn.readFrame()
		if err != nil {
			return fmt.Errorf("read from sdk: %w", err)
		}
		var msg Message
		if err := json.Unmarshal(frame, &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue
		}
		go s.dispatch(msg)
	}
}

func (s *StdioServer) dispatch(msg Message) {
	id := *msg.ID
	result, err := s.handleCall(msg.Method, msg.Params)
	if err != nil {
		s.conn.writeMessage(&Message{
			ID:    &id,
			Error: &Error{Code: -32603, Message: err.Error()},
		})
		return
	}
	raw, _ := json.Marshal(result)
	s.conn.writeMessage(&Message{
		ID:     &id,
		Result: raw,
	})
}

func (s *StdioServer) handleCall(method string, params json.RawMessage) (any, error) {
	if s.handler != nil {
		return s.handler(method, params)
	}

	// Default: forward to as-guestd
	result, err := s.agent.Call(method, params)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ForwardNotification sends a single notification to the SDK.
func (s *StdioServer) ForwardNotification(msg *Message) {
	if err := s.conn.writeMessage(msg); err != nil {
		log.Printf("forward notification: %v", err)
	}
}

// ForwardNotifications reads notifications from the as-guestd and forwards
// them to the SDK over stdout. Run in a goroutine.
func (s *StdioServer) ForwardNotifications() {
	if s.agent == nil {
		return
	}
	for notif := range s.agent.Notifications {
		s.ForwardNotification(notif)
	}
}
