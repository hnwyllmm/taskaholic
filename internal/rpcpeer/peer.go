// Package rpcpeer implements the small symmetric JSON-RPC 2.0 peer used over a
// WebSocket. Transport reliability is intentionally handled above this layer.
package rpcpeer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

type Request struct {
	ID     string
	Method string
	Params json.RawMessage
}

type Handler func(context.Context, Request) (any, *Error)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type callResult struct {
	result json.RawMessage
	err    error
}

type Peer struct {
	conn    *websocket.Conn
	handler Handler

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan callResult
	closed  chan struct{}
	once    sync.Once
	nextID  atomic.Uint64
}

func New(conn *websocket.Conn, handler Handler) *Peer {
	conn.SetReadLimit(4 << 20)
	return &Peer{
		conn:    conn,
		handler: handler,
		pending: make(map[string]chan callResult),
		closed:  make(chan struct{}),
	}
}

func (p *Peer) Serve(ctx context.Context) error {
	defer p.shutdown(errors.New("json-rpc peer closed"))
	for {
		var msg message
		if err := wsjson.Read(ctx, p.conn, &msg); err != nil {
			return err
		}
		if msg.JSONRPC != "2.0" {
			continue
		}
		if msg.Method != "" {
			req, err := requestFromMessage(msg)
			if err != nil {
				if len(msg.ID) != 0 {
					_ = p.writeResponse(ctx, "", nil, &Error{Code: -32600, Message: err.Error()}, msg.ID)
				}
				continue
			}
			go p.handle(ctx, req, msg.ID)
			continue
		}
		id, err := decodeID(msg.ID)
		if err != nil || id == "" {
			continue
		}
		p.mu.Lock()
		ch := p.pending[id]
		if ch != nil {
			delete(p.pending, id)
		}
		p.mu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- callResult{err: msg.Error}
		} else {
			ch <- callResult{result: msg.Result}
		}
	}
}

func (p *Peer) handle(ctx context.Context, req Request, rawID json.RawMessage) {
	if p.handler == nil {
		if req.ID != "" {
			_ = p.writeResponse(ctx, req.ID, nil, &Error{Code: -32601, Message: "method not found"}, rawID)
		}
		return
	}
	result, rpcErr := p.handler(ctx, req)
	if req.ID == "" {
		return
	}
	_ = p.writeResponse(ctx, req.ID, result, rpcErr, rawID)
}

func (p *Peer) Call(ctx context.Context, method string, params, result any) error {
	id := strconv.FormatUint(p.nextID.Add(1), 10)
	return p.CallID(ctx, id, method, params, result)
}

func (p *Peer) CallID(ctx context.Context, id, method string, params, result any) error {
	if id == "" {
		return errors.New("json-rpc call requires an id")
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	idJSON, _ := json.Marshal(id)
	ch := make(chan callResult, 1)

	p.mu.Lock()
	if _, exists := p.pending[id]; exists {
		p.mu.Unlock()
		return fmt.Errorf("json-rpc id already pending: %s", id)
	}
	p.pending[id] = ch
	p.mu.Unlock()

	msg := message{JSONRPC: "2.0", ID: idJSON, Method: method, Params: paramsJSON}
	if err := p.write(ctx, msg); err != nil {
		p.removePending(id)
		return err
	}

	select {
	case reply := <-ch:
		if reply.err != nil {
			return reply.err
		}
		if result == nil || len(reply.result) == 0 || string(reply.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(reply.result, result); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
		return nil
	case <-ctx.Done():
		p.removePending(id)
		return ctx.Err()
	case <-p.closed:
		return errors.New("json-rpc peer closed")
	}
}

func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	return p.write(ctx, message{JSONRPC: "2.0", Method: method, Params: paramsJSON})
}

func (p *Peer) Close(status websocket.StatusCode, reason string) error {
	p.shutdown(errors.New("json-rpc peer closed"))
	return p.conn.Close(status, reason)
}

func (p *Peer) writeResponse(ctx context.Context, id string, result any, rpcErr *Error, rawID json.RawMessage) error {
	msg := message{JSONRPC: "2.0", ID: rawID, Error: rpcErr}
	if rpcErr == nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			msg.Error = &Error{Code: -32603, Message: "encode response"}
		} else {
			msg.Result = encoded
		}
	}
	return p.write(ctx, msg)
}

func (p *Peer) write(ctx context.Context, msg message) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return wsjson.Write(ctx, p.conn, msg)
}

func (p *Peer) removePending(id string) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *Peer) shutdown(err error) {
	p.once.Do(func() {
		close(p.closed)
		p.mu.Lock()
		for id, ch := range p.pending {
			delete(p.pending, id)
			ch <- callResult{err: err}
		}
		p.mu.Unlock()
	})
}

func requestFromMessage(msg message) (Request, error) {
	id, err := decodeID(msg.ID)
	if err != nil {
		return Request{}, err
	}
	return Request{ID: id, Method: msg.Method, Params: msg.Params}, nil
}

func decodeID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var numeric json.Number
	if err := json.Unmarshal(raw, &numeric); err == nil {
		return numeric.String(), nil
	}
	return "", errors.New("invalid json-rpc id")
}
