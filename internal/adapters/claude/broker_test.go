package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meshra/internal/model"
)

// This SDK fixture exercises the actual bridge, stdio helper and scoped Unix
// HTTP transport, without starting a native query or contacting a provider.
const brokerSDKFixture = `
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
export const BUILTIN_TOOL_NAMES = ['Read','Glob','Grep','Bash','Edit','Write','Agent','Skill'];
export const LEGACY_TOOL_NAME_ALIASES = {Task:'Agent'};
export async function resolveSettings() {
  return {sources: process.env.MESHRA_TEST_SCENARIO === 'managed' ? [{source:'managed',settings:{hooks:{SessionStart:[]}}}] : []};
}
export function query({options}) {
  async function* messages() {
    const expected = ['mcp__meshra__meshra_workspace_read','mcp__meshra__meshra_profile_request'];
    if (options.tools !== undefined || options.allowedTools !== undefined || options.permissionMode !== 'bypassPermissions' || !options.allowDangerouslySkipPermissions ||
        options.skills.length || options.plugins.length || Object.keys(options.agents).length || options.settingSources.length || !options.strictMcpConfig ||
        options.settings.disableAllHooks || options.persistSession !== true || options.promptSuggestions || options.agentProgressSummaries ||
        Object.keys(options.env).some(key => key.startsWith('MESHRA_'))) throw new Error('Ambient profile authority');
    const config = options.mcpServers.meshra;
    if (Object.keys(options.mcpServers).join() !== 'meshra' || !config.command || config.url || config.headers ||
        config.env.MESHRA_TOOL_TOKEN !== 'attempt-only' || !config.env.MESHRA_TOOL_SOCKET.endsWith('.sock')) throw new Error('Unscoped MCP configuration');
    const child = spawn(config.command, config.args, {env:{...process.env,...config.env},stdio:['pipe','pipe','ignore']});
    const pending = new Map();
    let sequence = 0;
    const lines = createInterface({input:child.stdout});
    lines.on('line', line => {
      const frame = JSON.parse(line), request = pending.get(frame.id);
      if (!request) return;
      pending.delete(frame.id);
      frame.error ? request.reject(new Error(frame.error.message)) : request.resolve(frame.result);
    });
    const abort = () => {
      for (const request of pending.values()) request.reject(new Error('SDK transport cancelled'));
      pending.clear(); child.kill('SIGTERM');
    };
    options.abortController.signal.addEventListener('abort', abort, {once:true});
    const send = (method, params, notification = false) => {
      const id = ++sequence;
      const frame = {jsonrpc:'2.0',method,params,...(notification ? {} : {id})};
      const reply = notification ? Promise.resolve() : new Promise((resolve,reject) => pending.set(id,{resolve,reject}));
      child.stdin.write(JSON.stringify(frame)+'\n');
      return reply;
    };
    try {
      await send('initialize',{protocolVersion:'2024-11-05',capabilities:{},clientInfo:{name:'fixture',version:'1'}});
      await send('notifications/initialized',{},true);
      const list = await send('tools/list',{});
      if (list.tools.map(tool => 'mcp__meshra__'+tool.name).join() !== expected.join()) throw new Error('Non-exact broker list');
      await options.hooks.SessionStart[0].hooks[0]({session_id:'native-broker'});
      for (const name of ['Read','Glob','Grep','Bash','Edit','Write','Agent','Task','Skill','mcp__foreign__read', ...expected]) {
        const hook = await options.hooks.PreToolUse[0].hooks[0]({session_id:'native-broker',tool_name:name,tool_use_id:name});
        const permission = await options.canUseTool(name,{}, {toolUseID:name});
        if (hook.hookSpecificOutput || permission.behavior !== 'allow') throw new Error('Full-access tool denied '+name);
      }
      yield {type:'system',subtype:'init',session_id:'native-broker',claude_code_version:'2.1.303',
        tools:process.env.MESHRA_TEST_SCENARIO === 'bad-roster' ? expected.slice(1) : ['Read','Bash',...expected],
        mcp_servers:[{name:'meshra',status:'connected'}],plugins:[],skills:[]};
      const result = send('tools/call',{name:'meshra_workspace_read',arguments:{path:'rooted.txt'}});
      if (process.env.MESHRA_TEST_SCENARIO === 'async') await send('notifications/cancelled',{requestId:sequence},true);
      const reply = await result;
      yield {type:'result',subtype:'success',session_id:'native-broker',result:reply.content[0].text};
    } finally {
      options.abortController.signal.removeEventListener('abort',abort);
      lines.close(); child.kill('SIGTERM');
    }
  }
  const result = messages(); result.close = () => {}; return result;
}
`

func sdkFixtureAdapter(t *testing.T, scenario string) model.Adapter {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil { t.Skip("Node.js is required for the scoped SDK transport boundary") }
	if runtime.GOOS == "windows" { t.Skip("Unix socket transport requires a POSIX host") }
	_, source, _, _ := runtime.Caller(0)
	bridge, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../bridges/claude-agent-sdk/bridge.mjs"))
	if err != nil { t.Fatal(err) }
	root := t.TempDir()
	arch := runtime.GOARCH
	if arch == "amd64" { arch = "x64" }
	module := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk")
	native := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk-"+runtime.GOOS+"-"+arch)
	for _, dir := range []string{module, native} {
		if err = os.MkdirAll(dir, 0700); err != nil { t.Fatal(err) }
	}
	version := "0.3.303"
	if scenario == "unsupported" { version = "0.0.0" }
	files := map[string]string{
		filepath.Join(root, "bridge.mjs"): string(bridge),
		filepath.Join(module, "package.json"): fmt.Sprintf(`{"name":"@anthropic-ai/claude-agent-sdk","version":%q,"type":"module","exports":"./sdk.mjs"}`, version),
		filepath.Join(module, "sdk.mjs"): brokerSDKFixture,
		filepath.Join(native, "package.json"): `{"version":"0.3.303"}`,
		filepath.Join(native, "claude"): "#!/bin/sh\nprintf '2.1.303 (Claude Code)\\n'\n",
	}
	for path, data := range files {
		if err = os.WriteFile(path, []byte(data), 0700); err != nil { t.Fatal(err) }
	}
	return adapter(func(context.Context) (commandFactory, error) {
		return func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, node, filepath.Join(root, "bridge.mjs"))
			cmd.Env = append(os.Environ(), "MESHRA_TEST_SCENARIO="+scenario, "MESHRA_MAIN_TOKEN=main-bearer")
			return cmd
		}, nil
	})
}

func TestProfileSDKScopedTransport(t *testing.T) {
	for _, scenario := range []string{"normal", "async", "cancel", "bad-roster", "managed", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Keep the actual socket pathname below the portable Unix limit.
			dir, err := os.MkdirTemp("", "meshra-sdk-")
			if err != nil { t.Fatal(err) }
			defer os.RemoveAll(dir)
			broker := &scopedBroker{definitions: profileDefinitions(), socket: filepath.Join(dir, "tools.sock"), credential: "attempt-only"}
			listener, err := net.Listen("unix", broker.socket)
			if err != nil { t.Fatal(err) }
			var persisted atomic.Bool
			var earlyCalls atomic.Int32
			var calls atomic.Int32
			var listed atomic.Bool
			cancelled := make(chan struct{})
			var cancelOnce sync.Once
			httpServer := &http.Server{Handler:http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer attempt-only" {
					t.Error("native helper received a main or foreign bearer")
					w.WriteHeader(http.StatusUnauthorized); return
				}
				var frame struct { ID any `json:"id"`; Method string `json:"method"` }
				if err := json.NewDecoder(request.Body).Decode(&frame); err != nil { t.Error(err); w.WriteHeader(400); return }
				var result any
				switch frame.Method {
				case "initialize": result = map[string]any{"protocolVersion":"2024-11-05", "capabilities":map[string]any{"tools":map[string]any{}}, "serverInfo":map[string]string{"name":"fixture","version":"1"}}
				case "notifications/initialized": w.WriteHeader(202); return
				case "tools/list": listed.Store(true); result = map[string]any{"tools":broker.definitions}
				case "notifications/cancelled": cancelOnce.Do(func(){close(cancelled)}); w.WriteHeader(202); return
				case "tools/call":
					calls.Add(1)
					if !persisted.Load() { earlyCalls.Add(1) }
					if scenario == "cancel" { cancel(); <-request.Context().Done(); return }
					if scenario == "async" {
						select { case <-cancelled: case <-request.Context().Done(): return }
					}
					result = model.ToolResult{Content:[]model.ToolContent{{Type:"text",Text:"rooted-result"}}}
				default: t.Errorf("unexpected scoped MCP method %q",frame.Method); w.WriteHeader(400); return
				}
				w.Header().Set("Content-Type","application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc":"2.0","id":frame.ID,"result":result})
			})}
			go httpServer.Serve(listener)
			defer httpServer.Close()
			agent := model.Agent{ID:"claude:profile", Cwd:t.TempDir(), ToolDefinitions:profileDefinitions()}
			r, err := sdkFixtureAdapter(t,scenario).Factory(ctx,agent,nil)
			if err != nil { t.Fatal(err) }
			defer r.Close()
			answer, err := r.Run(ctx,model.Turn{Task:model.Task{To:agent.ID,Text:"broker transport"},Tools:broker,Token:"main-bearer",MCPURL:"http://main/mcp"},func(kind,data string){
				if kind == "session" { persisted.Store(true) }
				if strings.Contains(data,"attempt-only") || strings.Contains(data,"main-bearer") { t.Error("credential leaked in event") }
			})
			if earlyCalls.Load() != 0 { t.Fatal("scoped call preceded persisted native task session") }
			switch scenario {
			case "normal", "async":
				if err != nil || answer != "rooted-result" || calls.Load() != 1 || !listed.Load() { t.Fatalf("scoped SDK transport: answer=%q calls=%d listed=%v err=%v",answer,calls.Load(),listed.Load(),err) }
			case "cancel":
				if !errors.Is(err,context.Canceled) { t.Fatalf("approval cancellation: %v",err) }
			default:
				var unavailable *model.Error
				if !errors.As(err,&unavailable) || unavailable.Status != 503 || calls.Load() != 0 { t.Fatalf("unsafe SDK configuration dispatched a call: calls=%d err=%v",calls.Load(),err) }
			}
		})
	}
}
