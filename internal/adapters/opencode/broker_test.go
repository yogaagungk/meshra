package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meshra/internal/model"
)

type profileBroker struct {
	definitions []model.ToolDefinition
	allocations int
}
func (b *profileBroker) Definitions() []model.ToolDefinition { return b.definitions }
func (b *profileBroker) Call(context.Context, model.ToolCall) (model.ToolResult, error) {
	return model.ToolResult{}, errors.New("native transport must use the attempt endpoint")
}
func (b *profileBroker) ScopedMCP(context.Context) (string, string, error) {
	b.allocations++
	return "/private/attempt.sock", "attempt-only", nil
}
func profileDefinitions() []model.ToolDefinition {
	return []model.ToolDefinition{
		{Name:"meshra_workspace_read", Description:"Read within the selected root", InputSchema:json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name:"meshra_profile_request", Description:"Request an allowed profile", InputSchema:json.RawMessage(`{"type":"object","properties":{}}`)},
	}
}

func TestProfileRefusesUnsafeNativeStartup(t *testing.T) {
	for _, agent := range []model.Agent{
		{ToolDefinitions:[]model.ToolDefinition{}},
		{ToolDefinitions:profileDefinitions()},
		{ProfileManaged:true},
		{ManagedChat:true},
	} {
		resolved := false
		a := adapter(func()(starter,error){ resolved = true; return nil,errors.New("must not resolve native OpenCode") })
		if a.Capabilities["profileAutomation"] { t.Fatal("unsafe native profile capability advertised") }
		r, err := a.Factory(context.Background(),agent,nil)
		var unavailable *model.Error
		if r != nil || resolved || !errors.As(err,&unavailable) || unavailable.Status != 503 || !strings.Contains(err.Error(),"1.19.3") {
			t.Fatalf("profile crossed native startup gate: runtime=%v resolved=%v err=%v",r,resolved,err)
		}
		// Direct startup must not bypass the same fail-closed installation gate.
		if _, err = startServer("/must-not-execute")(context.Background(),agent,model.Turn{}); !errors.As(err,&unavailable) || unavailable.Status != 503 {
			t.Fatalf("direct unsafe native startup: %v",err)
		}
	}
}

func TestProfileStdioConfigurationUsesOnlyScopedCatalog(t *testing.T) {
	broker := &profileBroker{definitions:profileDefinitions()}
	config, credential, err := serverConfiguration(context.Background(),model.Agent{ToolDefinitions:profileDefinitions()},model.Turn{Tools:broker,Token:"main-bearer",MCPURL:"http://main/mcp"})
	if err != nil { t.Fatal(err) }
	permission := config["permission"].(map[string]any)
	if len(permission) != 3 || permission["*"] != "deny" || permission["meshra_meshra_workspace_read"] != "allow" || permission["meshra_meshra_profile_request"] != "allow" {
		t.Fatalf("not an exact replacement permission catalog: %#v",permission)
	}
	servers := config["mcp"].(map[string]any)
	if len(servers) != 1 { t.Fatal("foreign MCP configuration survived") }
	mcp := servers["meshra"].(map[string]any)
	command := mcp["command"].([]string)
	environment := mcp["environment"].(map[string]string)
	if mcp["type"] != "local" || len(command) != 2 || !filepath.IsAbs(command[1]) || !strings.HasSuffix(command[1],"bridges/mcp-stdio/bridge.mjs") ||
		environment["MESHRA_TOOL_SOCKET"] != "/private/attempt.sock" || environment["MESHRA_TOOL_TOKEN"] != credential || credential != "attempt-only" || broker.allocations != 1 {
		t.Fatalf("invalid scoped stdio contract: %#v",mcp)
	}
	raw, err := json.Marshal(config)
	if err != nil || strings.Contains(string(raw),"main-bearer") || strings.Contains(string(raw),"http://main/mcp") {
		t.Fatalf("main relay authority crossed the scoped transport boundary: %s %v",raw,err)
	}
}

// Simulate an installation that has passed a native isolation gate to exercise
// the backing HTTP/session transport. These fixtures do not enable or establish
// isolation of the installed OpenCode 1.19.3, whose public factory rejects it.
func profileRuntime(t *testing.T, f *fakeServer, session string) model.Runtime {
	t.Helper()
	agent := model.Agent{ID:"opencode:profile",Cwd:t.TempDir(),SessionID:session}
	if session != "" { f.sessions[session] = agent.Cwd }
	r, err := f.adapter().Factory(context.Background(),agent,nil)
	if err != nil { t.Fatal(err) }
	agent.ToolDefinitions = profileDefinitions()
	runtime := r.(*runtimeSession)
	runtime.agent = agent
	runtime.catalog, err = json.Marshal(agent.ToolDefinitions)
	if err != nil { t.Fatal(err) }
	f.profile = true
	return r
}

func TestProfileTransportReplacesLegacyPermissionsAndCredentials(t *testing.T) {
	f := newFake(t)
	f.policies["owned"] = []map[string]string{
		{"permission":"*","pattern":"*","action":"allow"},
		{"permission":"read","pattern":"*","action":"allow"},
		{"permission":"meshra_meshra_request","pattern":"*","action":"allow"},
	}
	r := profileRuntime(t,f,"owned")
	defer r.Close()
	broker := &profileBroker{definitions:profileDefinitions()}
	for range 2 {
		answer, err := r.Run(context.Background(),model.Turn{Task:model.Task{To:"opencode:profile",DestinationSessionID:"owned",Text:"profile"},Tools:broker,Token:"main-bearer",MCPURL:"http://main/mcp"},func(kind,data string){ if kind == "session" { f.persisted.Store(true) } })
		if err != nil || answer != "owned: profile" { t.Fatalf("profile transport: %q %v",answer,err) }
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches != 1 || f.created != 0 || f.prompts != 2 { t.Fatalf("incorrect profile resume lifecycle: patches=%d created=%d prompts=%d",f.patches,f.created,f.prompts) }
	for _, token := range f.tokens { if token != "opencode:profile:" { t.Fatalf("native process received main bearer: %q",token) } }
	policy := f.policies["owned"]
	tail := policy[len(policy)-3:]
	if tail[0]["permission"] != "*" || tail[0]["action"] != "deny" || tail[1]["permission"] != "meshra_meshra_workspace_read" || tail[2]["permission"] != "meshra_meshra_profile_request" {
		t.Fatalf("legacy native allowlist was extended rather than superseded: %#v",tail)
	}
}

func TestProfileTransportRejectsStaleAuthority(t *testing.T) {
	for _, scenario := range []string{"missing-broker","changed-catalog","foreign-task","stale-session","missing-emitter"} {
		t.Run(scenario,func(t *testing.T){
			f := newFake(t)
			r := profileRuntime(t,f,"owned")
			defer r.Close()
			broker := &profileBroker{definitions:profileDefinitions()}
			turn := model.Turn{Task:model.Task{To:"opencode:profile",DestinationSessionID:"owned",Text:"profile"},Tools:broker}
			emit := model.Emit(func(string,string){})
			switch scenario {
			case "missing-broker": turn.Tools = nil
			case "changed-catalog": broker.definitions[0].Description = "changed catalog"
			case "foreign-task": turn.Task.To = "opencode:other"
			case "stale-session": turn.Task.DestinationSessionID = "stale"
			case "missing-emitter": emit = nil
			}
			_, err := r.Run(context.Background(),turn,emit)
			var unavailable *model.Error
			if !errors.As(err,&unavailable) || unavailable.Status != 503 || f.starts.Load() != 0 || broker.allocations != 0 {
				t.Fatalf("stale authority reached native startup: starts=%d err=%v",f.starts.Load(),err)
			}
		})
	}
}

func TestProfileSupervisionAbortsNonBrokerTools(t *testing.T) {
	for _, tool := range []string{"read","bash","edit","write","task","foreign_mcp","meshra_meshra_request","meshra_meshra_collaboration_task_delegate"} {
		t.Run(tool,func(t *testing.T){
			f := newFake(t)
			r := profileRuntime(t,f,"")
			defer r.Close()
			ctx,cancel := context.WithTimeout(context.Background(),5*time.Second)
			defer cancel()
			_, err := r.Run(ctx,model.Turn{Task:model.Task{To:"opencode:profile",Text:"native:"+tool},Tools:&profileBroker{definitions:profileDefinitions()}},func(kind,data string){ if kind == "session" { f.persisted.Store(true) } })
			var unavailable *model.Error
			if !errors.As(err,&unavailable) || unavailable.Status != 503 { t.Fatalf("non-broker native tool was not aborted: %v",err) }
			select { case <-f.aborted: case <-time.After(time.Second): t.Fatal("native session was not aborted") }
		})
	}
}

func TestPendingNativeApprovalDoesNotBlockCancellation(t *testing.T) {
	f := newFake(t)
	f.blockDenial = true
	r,err := f.adapter().Factory(context.Background(),model.Agent{Cwd:t.TempDir()},nil)
	if err != nil { t.Fatal(err) }
	defer r.Close()
	ctx,cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error,1)
	go func(){ _,err := r.Run(ctx,model.Turn{Task:model.Task{Text:"denied"}},nil); done <- err }()
	select { case <-f.denialStarted: case <-time.After(5*time.Second): t.Fatal("native permission rejection did not start") }
	cancel()
	select { case err := <-done: if !errors.Is(err,context.Canceled) { t.Fatal(err) }; case <-time.After(5*time.Second): t.Fatal("pending native approval blocked cancellation") }
	select { case <-f.aborted: case <-time.After(time.Second): t.Fatal("cancelled native approval did not abort session") }
}
