package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Transport is the bounded JSONL framing of the ACP stdio transport. The
// repository's process transport satisfies it, so no second RPC framework or
// unbounded subprocess is introduced.
type Transport interface {
	Read(ctx context.Context) (json.RawMessage, error)
	Send(ctx context.Context, value any) error
	Close() error
}

// handler receives agent notifications and answers agent requests. Every
// request must be answered explicitly, including methods this client does not
// implement, so the agent never waits on a client that cannot reply.
type handler interface {
	notification(method string, params json.RawMessage)
	request(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)
}

type connection struct {
	transport Transport
	handler   handler
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	mu        sync.Mutex
	nextID    int64
	pending   map[string]chan *rpcMessage
	err       error
	closed    bool
	done      chan struct{}
	closeOnce sync.Once
	slots     chan struct{}
}

func newConnection(transport Transport, target handler) *connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &connection{transport: transport, handler: target, ctx: ctx, cancel: cancel, pending: map[string]chan *rpcMessage{}, done: make(chan struct{}), slots: make(chan struct{}, maxConcurrentClientCalls)}
	go c.readLoop()
	return c
}

// readLoop is the only reader of the transport. One oversized or malformed
// frame ends the connection instead of being retried or truncated.
func (c *connection) readLoop() {
	for {
		frame, err := c.transport.Read(c.ctx)
		if err != nil {
			c.fail(err)
			return
		}
		if len(frame) > MaxFrameBytes {
			c.fail(errors.New("ACP protocol frame exceeds 1 MiB"))
			return
		}
		var message rpcMessage
		if err = json.Unmarshal(frame, &message); err != nil {
			c.fail(errors.New("invalid ACP JSON-RPC frame"))
			return
		}
		c.dispatch(&message)
	}
}

func (c *connection) dispatch(message *rpcMessage) {
	if message.Method != "" {
		if len(message.ID) == 0 || string(message.ID) == "null" {
			c.handler.notification(message.Method, message.Params)
			return
		}
		c.serve(message)
		return
	}
	key := string(message.ID)
	c.mu.Lock()
	waiter := c.pending[key]
	c.mu.Unlock()
	if waiter == nil {
		return
	}
	select {
	case waiter <- message:
	default:
	}
}

// serve answers one agent request on its own goroutine. The number of
// concurrent client calls is bounded; excess requests receive an explicit
// error instead of being queued without limit.
func (c *connection) serve(message *rpcMessage) {
	id, valid := canonicalRequestID(message.ID)
	if !valid {
		c.respond(nil, nil, &rpcError{Code: -32600, Message: "invalid bounded ACP request identifier"})
		return
	}
	select {
	case c.slots <- struct{}{}:
	default:
		c.respond(message.ID, nil, &rpcError{Code: -32000, Message: "too many concurrent client requests"})
		return
	}
	go func() {
		defer func() { <-c.slots }()
		ctx := context.WithValue(c.ctx, clientRequestIDKey{}, id)
		result, failure := c.handler.request(ctx, message.Method, message.Params)
		c.respond(message.ID, result, failure)
	}()
}

// respond delivers one response. It uses its own bounded context so a
// cancelled turn still receives the mandatory "cancelled" outcome.
func (c *connection) respond(id json.RawMessage, result any, failure *rpcError) {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if failure != nil {
		response["error"] = map[string]any{"code": failure.Code, "message": failure.Message}
	} else {
		response["result"] = result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.send(ctx, response)
}

func (c *connection) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = errors.New("ACP connection closed")
		}
		return nil, err
	}
	if len(c.pending) >= maxPendingRequests {
		c.mu.Unlock()
		return nil, errors.New("too many in-flight ACP requests")
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	waiter := make(chan *rpcMessage, 1)
	c.pending[key] = waiter
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case message := <-waiter:
		if message.Error != nil {
			return nil, fmt.Errorf("ACP %s failed: %s", method, message.Error.Message)
		}
		return message.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = errors.New("ACP connection closed")
		}
		return nil, err
	}
}

func (c *connection) notify(ctx context.Context, method string, params any) error {
	return c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *connection) send(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameBytes {
		return errors.New("outbound ACP protocol frame exceeds 1 MiB")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.transport.Send(ctx, json.RawMessage(body))
}

func (c *connection) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.shutdown()
}

func (c *connection) shutdown() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.cancel()
		close(c.done)
		_ = c.transport.Close()
	})
}
