package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

// The parent claim is constructed by the fixture; the nested requests go
// through the real active broker, catalog, invocation store and attention flow.
func TestCodeBridgeNestedCallsUseActiveBroker(t *testing.T) {
	var r *Relay
	result := make(chan error, 1)
	run := func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		b, ok := turn.Tools.(*toolBroker)
		if !ok {
			return "", fmt.Errorf("missing active broker")
		}
		r.mu.Lock()
		_, _, epoch, err := b.activeLocked()
		inv := toolInvocation{ID: randomID(), TaskID: turn.Task.ID, Attempt: turn.Task.Attempts, CallID: randomID(), Name: "meshra_execute_code", ActionHash: tokenHash(randomID()), State: "pending", ProfileID: b.snapshot.Profile.ID, Epoch: epoch}
		if err == nil {
			_, err = r.db.Exec("INSERT INTO tool_invocations(id,task_id,attempt,call_id,action_hash,state,body) VALUES(?,?,?,?,?,'pending','{}')", inv.ID, inv.TaskID, inv.Attempt, inv.CallID, inv.ActionHash)
		}
		r.mu.Unlock()
		if err != nil {
			result <- err
			return "", err
		}
		parent := &brokerCall{invocation: inv, action: &toolAction{}}
		bridge, err := newCodeBridge(ctx, b, parent, nil)
		if err != nil {
			result <- err
			return "", err
		}
		defer bridge.Close()
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", bridge.listener.Addr().String())
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
		call := func(body string) (string, error) {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/call", strings.NewReader(body))
			if err != nil {
				return "", err
			}
			response, err := client.Do(request)
			if err != nil {
				return "", err
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			return string(data), err
		}
		denied, err := call(`{"name":"meshra_memory_save","arguments":{"content":"not granted"}}`)
		if err == nil && !strings.Contains(denied, "not in this admitted catalog") {
			err = fmt.Errorf("ungranted tool accepted: %s", denied)
		}
		if err == nil {
			var response string
			response, err = call(`{"name":"meshra_ask_user","arguments":{"question":"Nested question?"}}`)
			if err == nil && !strings.Contains(response, "nested answer") {
				err = fmt.Errorf("missing authorized answer: %s", response)
			}
		}
		result <- err
		return "complete", err
	}
	r, _ = continuityOpenTest(t, run)
	console := brokerConsole(r, "nested-code-owner")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "exercise nested broker")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	if _, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "nested answer"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("nested broker did not finish")
	}
	var count int
	if err := r.db.QueryRow("SELECT count(*) FROM tool_invocations WHERE task_id=? AND json_extract(body,'$.name')=?", task.ID, "meshra_ask_user").Scan(&count); err != nil || count != 1 {
		t.Fatalf("nested invocation was not audited: %d %v", count, err)
	}
}
