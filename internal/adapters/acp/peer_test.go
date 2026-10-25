package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// pipeTransport is the bounded JSONL framing of the ACP stdio transport. It
// carries the same newline-delimited JSON frames as the repository's process
// transport, so the fake peer exercises the real framing.
type pipeTransport struct {
	reader   *os.File
	writer   *os.File
	buffered *bufio.Reader
	mu       sync.Mutex
	closed   bool
}

func newPipePair() (*pipeTransport, *pipeTransport) {
	clientRead, peerWrite, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	peerRead, clientWrite, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	client := &pipeTransport{reader: clientRead, writer: clientWrite, buffered: bufio.NewReaderSize(clientRead, 64*1024)}
	peer := &pipeTransport{reader: peerRead, writer: peerWrite, buffered: bufio.NewReaderSize(peerRead, 64*1024)}
	return client, peer
}

func (t *pipeTransport) Read(ctx context.Context) (json.RawMessage, error) {
	for {
		line, err := t.buffered.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			return json.RawMessage(trimmed), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (t *pipeTransport) Send(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("transport closed")
	}
	_, err = t.writer.Write(body)
	return err
}

func (t *pipeTransport) writeRaw(frame []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("transport closed")
	}
	_, err := t.writer.Write(frame)
	return err
}

func (t *pipeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	_ = t.reader.Close()
	return t.writer.Close()
}

type peerRequest struct {
	Method string
	Params json.RawMessage
	id     json.RawMessage
}

// fakePeer is an in-process ACP agent. It speaks the same JSONL framing as a
// real CLI and never requires an installed ACP binary.
type fakePeer struct {
	transport     *pipeTransport
	mu            sync.Mutex
	nextID        int64
	pending       map[string]chan rpcMessage
	notifications chan string
	received      [][]byte
	handler       func(p *fakePeer, request peerRequest) (any, *rpcError)
	done          chan struct{}
}

func newFakePeer() (*fakePeer, *pipeTransport) {
	client, peer := newPipePair()
	p := &fakePeer{transport: peer, pending: map[string]chan rpcMessage{}, notifications: make(chan string, 32), done: make(chan struct{})}
	go p.loop()
	return p, client
}

func (p *fakePeer) loop() {
	defer close(p.done)
	for {
		frame, err := p.transport.Read(context.Background())
		if err != nil {
			return
		}
		p.mu.Lock()
		p.received = append(p.received, append([]byte(nil), frame...))
		p.mu.Unlock()
		var message rpcMessage
		if err = json.Unmarshal(frame, &message); err != nil {
			continue
		}
		if message.Method != "" {
			if len(message.ID) == 0 {
				select {
				case p.notifications <- message.Method:
				default:
				}
				continue
			}
			request := peerRequest{Method: message.Method, Params: message.Params, id: message.ID}
			go p.serve(request)
			continue
		}
		key := string(message.ID)
		p.mu.Lock()
		waiter := p.pending[key]
		delete(p.pending, key)
		p.mu.Unlock()
		if waiter != nil {
			waiter <- message
		}
	}
}

func (p *fakePeer) serve(request peerRequest) {
	var result any
	var failure *rpcError
	if p.handler == nil {
		failure = &rpcError{Code: -32601, Message: "unsupported method"}
	} else {
		result, failure = p.handler(p, request)
	}
	response := map[string]any{"jsonrpc": "2.0", "id": request.id}
	if failure != nil {
		response["error"] = map[string]any{"code": failure.Code, "message": failure.Message}
	} else {
		response["result"] = result
	}
	_ = p.transport.Send(context.Background(), response)
}

// notify sends an agent notification to the client.
func (p *fakePeer) notify(method string, params any) {
	_ = p.transport.Send(context.Background(), map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request asks the client something and waits for the answer.
func (p *fakePeer) request(t *testing.T, method string, params any) rpcMessage {
	t.Helper()
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	key := strconv.FormatInt(id, 10)
	waiter := make(chan rpcMessage, 1)
	p.pending[key] = waiter
	p.mu.Unlock()
	if err := p.transport.Send(context.Background(), map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		t.Errorf("send the agent request: %v", err)
		return rpcMessage{}
	}
	select {
	case message := <-waiter:
		return message
	case <-time.After(10 * time.Second):
		t.Errorf("the client did not answer %s", method)
		return rpcMessage{}
	}
}

// awaitNotification waits for one client notification.
func (p *fakePeer) awaitNotification(t *testing.T) string {
	t.Helper()
	select {
	case method := <-p.notifications:
		return method
	case <-time.After(10 * time.Second):
		t.Error("the client sent no notification")
		return ""
	}
}

// frames returns every frame the client sent.
func (p *fakePeer) frames() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, 0, len(p.received))
	out = append(out, p.received...)
	return out
}
