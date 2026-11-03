package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

func TestBrokerTodoLifecycleIsolationAndHarnessGate(t *testing.T) {
	type outcome struct {
		lists [][]todoItem
		err   error
	}
	results := make(chan outcome, 3)
	var turns atomic.Int32
	r, _ := continuityOpenTest(t, func(ctx context.Context, turn model.Turn, _ model.Emit) (string, error) {
		call := func(id, args string) ([]todoItem, error) {
			result, err := turn.Tools.Call(ctx, model.ToolCall{ID: id, Name: "meshra_todo", Arguments: json.RawMessage(args)})
			if err != nil {
				return nil, err
			}
			var items []todoItem
			if err = json.Unmarshal([]byte(brokerText(result)), &items); err != nil {
				return nil, err
			}
			return items, nil
		}
		o := outcome{}
		if turns.Add(1) == 1 {
			items, err := call("add", `{"action":"add","text":"first step"}`)
			if err == nil && (len(items) != 1 || items[0].ID == "" || items[0].Status != "pending") {
				err = fmt.Errorf("unexpected add result: %+v", items)
			}
			if err == nil {
				o.lists = append(o.lists, items)
				args, _ := json.Marshal(map[string]string{"action": "update", "id": items[0].ID, "status": "done"})
				items, err = call("update", string(args))
			}
			if err == nil {
				o.lists = append(o.lists, items)
				items, err = call("list", `{"action":"list"}`)
			}
			if err == nil {
				o.lists = append(o.lists, items)
			}
			o.err = err
		} else {
			items, err := call("list", `{"action":"list"}`)
			o.lists = append(o.lists, items)
			o.err = err
		}
		results <- o
		return "completed", nil
	})
	r.SetMCPURL("http://127.0.0.1:1/mcp")
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("fake harness unexpectedly called the endpoint")
	}))
	defer upstream.Close()
	r.mu.Lock()
	r.adapters["openai"] = r.adapters["omp"]
	r.mu.Unlock()
	saved, err := connectionsCall(t, r, admin, "connections.saveEndpoint", map[string]any{"endpoint": map[string]any{"name": "Todo fixture", "baseUrl": upstream.URL, "allowInsecureHttp": true, "enabled": true, "supportsTools": true}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := saved.(ProviderEndpointView)
	if _, err = networkCall(t, r, admin, "network.grant", map[string]any{"connectionId": endpoint.ID, "origin": upstream.URL, "allowPrivate": true}); err != nil {
		t.Fatal(err)
	}
	request := profileStoreRequest(profileStoreWorkspace(t, r))
	request["binding"] = model.ProfileProvider{Harness: "openai", ConnectionID: endpoint.ID, Config: model.Config{Provider: endpoint.ID, Model: "fixture"}}
	profile := profileStoreCreate(t, r, request)
	conversation := continuityConversation(t, r, "chat", profile)
	awaitResult := func() outcome {
		t.Helper()
		select {
		case result := <-results:
			return result
		case <-time.After(10 * time.Second):
			t.Fatal("todo broker turn did not execute")
			return outcome{}
		}
	}
	awaitTask(t, r, continuitySend(t, r, profile, conversation, "first turn").ID)
	first := awaitResult()
	if first.err != nil || len(first.lists) != 3 {
		t.Fatalf("todo lifecycle failed: %+v", first)
	}
	for _, items := range first.lists[1:] {
		if len(items) != 1 || items[0].ID != first.lists[0][0].ID || items[0].Text != "first step" || items[0].Status != "done" {
			t.Fatalf("todo update/list mismatch: %+v", items)
		}
	}
	awaitTask(t, r, continuitySend(t, r, profile, conversation, "second turn").ID)
	second := awaitResult()
	if second.err != nil || len(second.lists) != 1 || len(second.lists[0]) != 0 {
		t.Fatalf("todo leaked between tasks: %+v", second)
	}
	other := profileStoreCreate(t, r, profileStoreRequest(profileStoreWorkspace(t, r)))
	otherConversation := continuityConversation(t, r, "chat", other)
	awaitTask(t, r, continuitySend(t, r, other, otherConversation, "native turn").ID)
	third := awaitResult()
	if brokerStatus(third.err) != 403 {
		t.Fatalf("non-Direct API harness received todo: %v", third.err)
	}
}
