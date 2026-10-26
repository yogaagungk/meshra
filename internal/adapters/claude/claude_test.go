package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

// The subprocess exercises the real JSONL/process boundary without an SDK,
// provider credentials, network, or paid turn.
func TestBridgeProcess(t *testing.T) {
	if os.Getenv("MESHRA_FAKE_CLAUDE") != "1" {
		return
	}
	var input struct {
		ToolDefinitions                      []model.ToolDefinition
		ToolSocket, ToolToken, ToolBridge      string
		Prompt, Cwd, SessionID, Token, MCPURL string
		GenerationDraft                       bool
	}
	decoder := json.NewDecoder(os.Stdin)
	if err := decoder.Decode(&input); err != nil {
		os.Exit(2)
	}
	emit := func(kind, data string) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"type": kind, "data": data})
	}
	id := input.SessionID
	if id == "" {
		id = "native-" + filepath.Base(input.Cwd)
	}
	if input.Prompt == "mismatch" {
		id = "someone-elses-session"
	}
	emit("session", id)
	if input.ToolDefinitions != nil {
		var ack struct { Type, Data string }
		if input.Token != "" || input.MCPURL != "" || !filepath.IsAbs(input.ToolBridge) ||
			!strings.HasSuffix(input.ToolSocket, ".sock") || input.ToolToken == "" ||
			decoder.Decode(&ack) != nil || ack.Type != "session_ready" || ack.Data != id {
			emit("error", "profile did not receive scoped authority after session persistence")
			os.Exit(1)
		}
		emit("progress", "broker-dispatched")
	}
	switch input.Prompt {
	case "draft":
		if !input.GenerationDraft || input.MCPURL != "" || input.Token != "" || input.SessionID != "" {
			emit("error", "draft received ambient session or broker authority")
			break
		}
		emit("result", `{"name":"Draft","instructions":"Be helpful."}`)
	case "wait":
		time.Sleep(time.Minute)
	case "bad-json":
		fmt.Println("not json")
	case "missing-result":
	case "error":
		emit("error", "provider authentication rejected "+input.Token)
	case "profile":
		emit("result", "broker "+input.ToolToken)
	default:
		emit("approval_required", "Denied tool Bash")
		emit("delta", "answer ")
		emit("result", id+": "+input.Prompt+" "+input.Token)
	}
	os.Exit(0)
}
func fakeAdapter(started *atomic.Int32) model.Adapter {
	return adapter(func(context.Context) (commandFactory, error) {
		return func(ctx context.Context) *exec.Cmd {
			started.Add(1)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBridgeProcess$")
			cmd.Env = append(os.Environ(), "MESHRA_FAKE_CLAUDE=1")
			return cmd
		}, nil
	})
}
func TestSessionAllocationResumeIsolation(t *testing.T) {
	var started atomic.Int32
	a := fakeAdapter(&started)
	one, err := a.Factory(context.Background(), model.Agent{ID: "claude:one", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := a.Factory(context.Background(), model.Agent{ID: "claude:two", Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if started.Load() != 0 || one.SessionID() != "" || two.SessionID() != "" {
		t.Fatal("factory ran a turn or invented a session")
	}
	var events []string
	emit := func(kind, data string) { events = append(events, kind+":"+data) }
	first, err := one.Run(context.Background(), model.Turn{Task: model.Task{Text: "first"}, Token: "secret"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if one.SessionID() == "" || strings.Contains(first, "secret") || !strings.Contains(first, "[redacted]") {
		t.Fatalf("bad first response %q", first)
	}
	if len(events) < 3 || !strings.HasPrefix(events[0], "session:") || !strings.HasPrefix(events[1], "approval_required:") {
		t.Fatalf("event sequence: %v", events)
	}
	id := one.SessionID()
	second, err := one.Run(context.Background(), model.Turn{Task: model.Task{Text: "second"}}, nil)
	if err != nil || !strings.HasPrefix(second, id+":") || one.SessionID() != id {
		t.Fatalf("resume failed: %q %v", second, err)
	}
	if _, err = two.Run(context.Background(), model.Turn{Task: model.Task{Text: "other"}}, nil); err != nil {
		t.Fatal(err)
	}
	if one.SessionID() == two.SessionID() {
		t.Fatal("independent identities shared a native session")
	}
}
func TestManagedChatRunsWithoutProfileCatalog(t *testing.T) {
	var started atomic.Int32
	a := fakeAdapter(&started)
	r, err := a.Factory(context.Background(), model.Agent{ID: "claude:chat", Cwd: t.TempDir(), ManagedChat: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "hello"}}, nil); err != nil || started.Load() != 1 {
		t.Fatalf("managed chat did not run: starts=%d err=%v", started.Load(), err)
	}
	_, err = a.Factory(context.Background(), model.Agent{ID: "claude:profile", Cwd: t.TempDir(), ProfileManaged: true}, nil)
	var unavailable *model.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 503 {
		t.Fatalf("profile without catalog was admitted: %v", err)
	}
}
func TestBridgeFailures(t *testing.T) {
	for _, prompt := range []string{"bad-json", "missing-result", "error", "mismatch"} {
		t.Run(prompt, func(t *testing.T) {
			var started atomic.Int32
			r, err := fakeAdapter(&started).Factory(context.Background(), model.Agent{Cwd: t.TempDir(), SessionID: "owned-session"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: prompt}, Token: "credential"}, nil)
			if err == nil || strings.Contains(err.Error(), "credential") {
				t.Fatalf("expected sanitized failure, got %v", err)
			}
			if r.SessionID() != "owned-session" {
				t.Fatal("failure remapped owned session")
			}
		})
	}
}
func TestCancelReapsBridgeAndPreservesSession(t *testing.T) {
	var started atomic.Int32
	life, stop := context.WithCancel(context.Background())
	defer stop()
	r, err := fakeAdapter(&started).Factory(life, model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "wait"}}, func(kind, data string) {
			if kind == "session" {
				cancel()
			}
		})
		done <- err
	}()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge not reaped after cancellation")
	}
	if r.SessionID() == "" {
		t.Fatal("allocated session was not retained")
	}
	if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "resumed"}}, nil); err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err = r.Run(context.Background(), model.Turn{Task: model.Task{Text: "after-stop"}}, nil); err == nil {
		t.Fatal("daemon cancellation ignored")
	}
}
func TestCloseCancelsTurn(t *testing.T) {
	var started atomic.Int32
	r, err := fakeAdapter(&started).Factory(context.Background(), model.Agent{Cwd: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	allocated := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), model.Turn{Task: model.Task{Text: "wait"}}, func(kind, data string) {
			if kind == "session" {
				allocated <- struct{}{}
			}
		})
		done <- err
	}()
	select {
	case <-allocated:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not allocate")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not reap bridge")
	}
	if _, err = r.Run(context.Background(), model.Turn{}, nil); err == nil {
		t.Fatal("closed runtime accepted a turn")
	}
}
func TestAuthenticatedSmoke(t *testing.T) {
	if os.Getenv("MESHRA_AUTH_SMOKE") != "1" {
		t.Skip("opt-in paid Claude SDK smoke; see docs/adapters-sdk.md")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := New()
	r, err := a.Factory(ctx, model.Agent{ID: "claude:smoke", Cwd: t.TempDir(), Config: model.Config{Model: os.Getenv("MESHRA_CLAUDE_MODEL")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 2 {
		text, err := r.Run(ctx, model.Turn{Task: model.Task{Text: "Reply exactly MESHRA_SMOKE_OK. Do not use tools."}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "MESHRA_SMOKE_OK") || r.SessionID() == "" {
			t.Fatalf("unexpected smoke result %q", text)
		}
	}
}

func TestGenerationDraftBridgeBoundary(t *testing.T) {
	var started atomic.Int32
	a := fakeAdapter(&started)
	if !a.Capabilities["toolFreeDraft"] || !a.Capabilities["durableHistory"] {
		t.Fatal("missing supported Claude mode capabilities")
	}
	if _, err := a.Factory(context.Background(), model.Agent{GenerationDraft: true, SessionID: "legacy"}, nil); err == nil || started.Load() != 0 {
		t.Fatal("generation reused an existing native session")
	}
	r, err := a.Factory(context.Background(), model.Agent{Cwd: t.TempDir(), GenerationDraft: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Run(context.Background(), model.Turn{Task: model.Task{Text: "draft"}, MCPURL: "http://127.0.0.1/mcp", Token: "must-not-cross"}, nil)
	if err != nil || !strings.Contains(got, `"name":"Draft"`) {
		t.Fatalf("tool-free bridge payload: %q %v", got, err)
	}
}

// Exercise the actual bridge's hook and permission handlers at the SDK boundary:
// attempted reads and unknown calls must be denied before any tool can run.
func TestGenerationDraftSDKDenials(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required to exercise the Claude bridge policy")
	}
	_, source, _, _ := runtime.Caller(0)
	bridge, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../bridges/claude-agent-sdk/bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"draft", "legacy", "managed", "unsupported", "unexpected-roster", "resume", "preflight"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk")
			if err := os.MkdirAll(module, 0700); err != nil {
				t.Fatal(err)
			}
			version := "0.3.303"
			if scenario == "unsupported" {
				version = "0.0.0"
			}
			files := map[string]string{
				filepath.Join(root, "bridge.mjs"):     string(bridge),
				filepath.Join(module, "package.json"): fmt.Sprintf(`{"name":"@anthropic-ai/claude-agent-sdk","version":%q,"type":"module","exports":"./sdk.mjs"}`, version),
				filepath.Join(module, "sdk.mjs"): `
export const BUILTIN_TOOL_NAMES = ['Read', 'Glob', 'Grep', 'Bash', 'Edit', 'Write', 'Agent', 'Skill'];
export const LEGACY_TOOL_NAME_ALIASES = {Task: 'Agent'};
export async function resolveSettings() {
  return {sources: process.env.MESHRA_TEST_SCENARIO === 'managed' ? [{source:'managed',settings:{hooks:{SessionStart:[]}}}] : []};
}
export function query({options}) {
  if (process.env.MESHRA_TEST_SCENARIO === 'preflight') throw new Error('Preflight opened a native query');
  const draft = process.env.MESHRA_TEST_SCENARIO !== 'legacy';
  async function* messages() {
    const native = ['Read','Glob','Grep','Bash','Edit','Write','Agent','Task','Skill','mcp__foreign__read','mcp__meshra__meshra_send','unknown'];
    let denied = 0;
    for (const name of native) {
      const hook = await options.hooks.PreToolUse[0].hooks[0]({tool_name:name,tool_use_id:name});
      const permission = await options.canUseTool(name,{path:'/outside/sentinel'},{toolUseID:name});
      const allow = !draft && ['Read','Glob','Grep','mcp__meshra__meshra_send'].includes(name);
      if (allow ? hook.hookSpecificOutput || permission.behavior !== 'allow' :
          hook.hookSpecificOutput?.permissionDecision !== 'deny' || permission.behavior !== 'deny') {
        throw new Error('Unsafe hook/permission decision for '+name);
      }
      if (!allow) denied++;
    }
    if (draft) {
      for (const key of ['tools','allowedTools','skills','plugins']) {
        if (!Array.isArray(options[key]) || options[key].length) throw new Error('Ambient '+key);
      }
      if (Object.keys(options.mcpServers).length || Object.keys(options.agents).length ||
          options.settingSources.length || !options.strictMcpConfig || options.persistSession ||
          !options.settings.disableAllHooks || options.settings.autoMemoryEnabled !== false ||
          !options.disallowedTools.includes('mcp__*')) throw new Error('Ambient authority');
    } else if (!options.mcpServers.meshra || options.tools !== undefined || !options.persistSession) {
      throw new Error('Legacy mode changed');
    }
    yield {type:'system',subtype:'init',session_id:'native-draft',claude_code_version:'2.1.303',tools:process.env.MESHRA_TEST_SCENARIO === 'unexpected-roster' ? ['Read'] : [],mcp_servers:[],plugins:[],skills:[]};
    yield {type:'result',subtype:'success',session_id:'native-draft',result:JSON.stringify({denied})};
  }
  const result = messages();
  result.close = () => {};
  return result;
}
`,
			}
			for path, contents := range files {
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input := map[string]any{"prompt": "Generate a profile", "cwd": root, "generationDraft": scenario != "legacy", "mcpURL": "http://127.0.0.1/mcp", "token": "scoped"}
			if scenario == "resume" {
				input["sessionId"] = "legacy-session"
			}
			if scenario == "preflight" {
				input["checkGenerationDraft"] = true
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, filepath.Join(root, "bridge.mjs"))
			cmd.Env = append(os.Environ(), "MESHRA_TEST_SCENARIO="+scenario)
			cmd.Stdin = strings.NewReader(string(payload) + "\n")
			out, err := cmd.CombinedOutput()
			failure := scenario != "draft" && scenario != "legacy" && scenario != "preflight"
			if failure {
				if err == nil || !strings.Contains(string(out), `"type":"error"`) || strings.Contains(string(out), `"type":"result"`) {
					t.Fatalf("unsafe native initialization accepted: %s (%v)", out, err)
				}
			} else if err != nil || !strings.Contains(string(out), `"type":"result"`) {
				t.Fatalf("bridge policy failed: %s (%v)", out, err)
			}
		})
	}
}

type scopedBroker struct {
	definitions []model.ToolDefinition
	socket string
	credential string
	allocations atomic.Int32
}

func (b *scopedBroker) Definitions() []model.ToolDefinition { return b.definitions }
func (b *scopedBroker) Call(context.Context, model.ToolCall) (model.ToolResult, error) {
	return model.ToolResult{}, errors.New("native adapter must use the scoped endpoint, not call an unbound executor")
}
func (b *scopedBroker) ScopedMCP(context.Context) (string, string, error) {
	b.allocations.Add(1)
	return b.socket, b.credential, nil
}
func profileDefinitions() []model.ToolDefinition {
	return []model.ToolDefinition{
		{Name: "meshra_workspace_read", Description: "Read within the selected root", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "meshra_profile_request", Description: "Request an allowed profile", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
	}
}

func TestProfileSessionPersistencePrecedesDispatch(t *testing.T) {
	var started atomic.Int32
	agent := model.Agent{ID: "claude:profile", Cwd: t.TempDir(), ToolDefinitions: profileDefinitions()}
	r, err := fakeAdapter(&started).Factory(context.Background(), agent, func(string, string) { t.Error("factory emitter cannot persist a deferred task session") })
	if err != nil { t.Fatal(err) }
	defer r.Close()
	broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(t.TempDir(), "attempt.sock"), credential: "attempt-only"}
	persisting := make(chan struct{})
	persisted := make(chan struct{})
	dispatched := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		answer, err := r.Run(context.Background(), model.Turn{Task: model.Task{To: agent.ID, Text: "profile"}, Tools: broker, MCPURL: "http://main/mcp", Token: "main-bearer"}, func(kind, data string) {
			if kind == "session" { close(persisting); <-persisted }
			if kind == "progress" && data == "broker-dispatched" { dispatched <- struct{}{} }
		})
		if err == nil && (strings.Contains(answer, broker.credential) || !strings.Contains(answer, "[redacted]")) {
			err = fmt.Errorf("scoped credential leaked in result %q", answer)
		}
		done <- err
	}()
	select { case <-persisting: case <-time.After(5*time.Second): t.Fatal("session was not emitted through Run") }
	select { case <-dispatched: t.Fatal("tool dispatch preceded persistence"); default: }
	close(persisted)
	select { case err := <-done: if err != nil { t.Fatal(err) }; case <-time.After(5*time.Second): t.Fatal("acknowledged bridge did not finish") }
	if r.SessionID() == "" || broker.allocations.Load() != 1 { t.Fatal("native session or scoped endpoint was not retained") }
	select { case <-dispatched: default: t.Fatal("acknowledged session did not dispatch") }
}

func TestProfileRejectsStaleAuthorityBeforeStartup(t *testing.T) {
	for _, scenario := range []string{"missing-broker", "changed-catalog", "foreign-task", "stale-session", "legacy-runtime", "missing-emitter"} {
		t.Run(scenario, func(t *testing.T) {
			var started atomic.Int32
			agent := model.Agent{ID: "claude:profile", Cwd: t.TempDir(), ToolDefinitions: profileDefinitions(), SessionID: "owned"}
			broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(t.TempDir(), "attempt.sock"), credential: "attempt-only"}
			turn := model.Turn{Task: model.Task{To: agent.ID, Text: "profile", DestinationSessionID: "owned"}, Tools: broker}
			emit := model.Emit(func(string, string) {})
			switch scenario {
			case "missing-broker": turn.Tools = nil
			case "changed-catalog": broker.definitions[0].InputSchema = json.RawMessage(`{"type":"object","additionalProperties":false}`)
			case "foreign-task": turn.Task.To = "claude:other"
			case "stale-session": turn.Task.DestinationSessionID = "stale"
			case "legacy-runtime": agent.ToolDefinitions = nil
			case "missing-emitter": emit = nil
			}
			r, err := fakeAdapter(&started).Factory(context.Background(), agent, nil)
			if err != nil { t.Fatal(err) }
			defer r.Close()
			_, err = r.Run(context.Background(), turn, emit)
			var unavailable *model.Error
			if !errors.As(err, &unavailable) || unavailable.Status != 503 || started.Load() != 0 || broker.allocations.Load() != 0 {
				t.Fatalf("stale authority crossed startup boundary: starts=%d allocations=%d err=%v", started.Load(), broker.allocations.Load(), err)
			}
		})
	}
}

func TestProfileCancellationWhilePersistingSession(t *testing.T) {
	var started atomic.Int32
	agent := model.Agent{ID: "claude:profile", Cwd: t.TempDir(), ToolDefinitions: profileDefinitions()}
	r, err := fakeAdapter(&started).Factory(context.Background(), agent, nil)
	if err != nil { t.Fatal(err) }
	defer r.Close()
	broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(t.TempDir(), "attempt.sock"), credential: "attempt-only"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = r.Run(ctx, model.Turn{Task: model.Task{To: agent.ID, Text: "profile"}, Tools: broker}, func(kind, data string) {
		if kind == "session" { cancel() }
		if kind == "progress" { t.Error("cancelled persistence dispatched a tool") }
	})
	if !errors.Is(err, context.Canceled) { t.Fatalf("cancelled session handshake: %v", err) }
}
