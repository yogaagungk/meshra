package toolmcp

import (
 "context"
 "encoding/json"
 "fmt"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync/atomic"
 "testing"
 "time"

 "meshra/internal/model"
)

// This executor tests protocol lifecycle only, not native or sandbox execution.
type protocolExecutor struct {
 started chan model.ToolCall
 finished chan string
 calls atomic.Int32
 block bool
}
func (e *protocolExecutor) Definitions() []model.ToolDefinition {
 return []model.ToolDefinition{{Name:"meshra_workspace_read",Description:"Read a scoped file",InputSchema:json.RawMessage(`{"type":"object"}`)}}
}
func (e *protocolExecutor) Call(ctx context.Context, call model.ToolCall) (model.ToolResult,error) {
 e.calls.Add(1)
 if e.started!=nil {e.started<-call}
 if e.block {<-ctx.Done();if e.finished!=nil{e.finished<-call.ID};return model.ToolResult{},ctx.Err()}
 return model.ToolResult{Content:[]model.ToolContent{{Type:"text",Text:"scoped result"}}},nil
}
type protocolFixture struct {
 handler *Handler
 executor *protocolExecutor
 expired atomic.Bool
}
func newProtocolFixture(t *testing.T, block bool) *protocolFixture {
 t.Helper()
 f:=&protocolFixture{executor:&protocolExecutor{started:make(chan model.ToolCall,64),finished:make(chan string,64),block:block}}
 f.handler=New(f.executor,func(_ context.Context,r *http.Request)error{
  if r.Header.Get("Authorization")!="Bearer attempt-token" {return model.Err(401,"invalid credential")}
  if f.expired.Load(){return model.Err(403,"expired attempt")};return nil
 })
 t.Cleanup(f.handler.Close)
 return f
}
func(f *protocolFixture) request(method,session,body string)*httptest.ResponseRecorder {
 r:=httptest.NewRequest(method,"http://tool.invalid/mcp",strings.NewReader(body))
 r.Header.Set("Authorization","Bearer attempt-token")
 if session!="" {r.Header.Set("Mcp-Session-Id",session)}
 w:=httptest.NewRecorder();f.handler.ServeHTTP(w,r);return w
}
func(f *protocolFixture) initialize(t *testing.T)string {
 t.Helper();w:=f.request("POST","",`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
 requireRPCCode(t,w,0)
 id:=w.Header().Get("Mcp-Session-Id");if id==""{t.Fatal("initialize omitted session")};return id
}
func requireRPCCode(t *testing.T,w *httptest.ResponseRecorder,code int) {
 t.Helper()
 var response struct{JSONRPC string `json:"jsonrpc"`;Error *struct{Code int `json:"code"`} `json:"error"`}
 if w.Code!=200||json.Unmarshal(w.Body.Bytes(),&response)!=nil||response.JSONRPC!="2.0"{t.Fatalf("not an RPC response: %d %s",w.Code,w.Body.String())}
 if code==0 {if response.Error!=nil{t.Fatalf("unexpected RPC error: %s",w.Body.String())};return}
 if response.Error==nil||response.Error.Code!=code{t.Fatalf("wanted RPC error %d: %s",code,w.Body.String())}
}
func takeStarted(t *testing.T,e *protocolExecutor)model.ToolCall {
 t.Helper();select{case call:=<-e.started:return call;case <-time.After(3*time.Second):t.Fatal("executor did not start");return model.ToolCall{}}
}
func takeFinished(t *testing.T,e *protocolExecutor)string {
 t.Helper();select{case id:=<-e.finished:return id;case <-time.After(3*time.Second):t.Fatal("executor was not cancelled");return ""}
}
func startCall(f *protocolFixture,session,id string)<-chan *httptest.ResponseRecorder {
 done:=make(chan *httptest.ResponseRecorder,1)
 go func(){done<-f.request("POST",session,fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{}}}`,id))}()
 return done
}
func takeResponse(t *testing.T,done <-chan *httptest.ResponseRecorder)*httptest.ResponseRecorder {
 t.Helper();select{case w:=<-done:return w;case <-time.After(3*time.Second):t.Fatal("RPC did not complete");return nil}
}
func TestExactActiveCatalogAndUnsupportedMethods(t *testing.T) {
 f:=newProtocolFixture(t,false);session:=f.initialize(t)
 w:=f.request("POST",session,`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
 requireRPCCode(t,w,0)
 var response struct{Result struct{Tools []model.ToolDefinition `json:"tools"`} `json:"result"`}
 if err:=json.Unmarshal(w.Body.Bytes(),&response);err!=nil{t.Fatal(err)}
 if len(response.Result.Tools)!=1||response.Result.Tools[0].Name!="meshra_workspace_read"{t.Fatalf("unexpected active catalog: %s",w.Body.String())}
 for _,method:=range []string{"resources/list","resources/read","prompts/list","sampling/createMessage","meshra_send","meshra_request","approvals.decide"} {
  t.Run(method,func(t *testing.T){requireRPCCode(t,f.request("POST",session,fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":%q,"params":{}}`,method)),-32601)})
 }
 for _,name:=range []string{"meshra_send","meshra_workspace_command","approvals.decide"}{
  requireRPCCode(t,f.request("POST",session,fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,name)),-32602)
 }
 if f.executor.calls.Load()!=0 {t.Fatal("unsupported calls reached executor")}
 requireRPCCode(t,f.request("POST",session,`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{}}}`),0)
 if f.executor.calls.Load()!=1{t.Fatal("catalog call was not dispatched exactly once")}
}
func TestAttemptAuthenticationAndExpiredSessions(t *testing.T) {
 f:=newProtocolFixture(t,false);session:=f.initialize(t)
 body:=`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{}}}`
 for _,token:=range []string{"","Bearer invalid"}{
  r:=httptest.NewRequest("POST","http://tool.invalid/mcp",strings.NewReader(body));r.Header.Set("Authorization",token);r.Header.Set("Mcp-Session-Id",session)
  w:=httptest.NewRecorder();f.handler.ServeHTTP(w,r);if w.Code!=401{t.Fatal("invalid credential accepted",w.Code)}
 }
 f.expired.Store(true)
 if w:=f.request("POST",session,body);w.Code!=403{t.Fatal("expired attempt accepted",w.Code)}
 f.expired.Store(false)
 for _,session:=range []string{"","wrong-session"}{if w:=f.request("POST",session,body);w.Code!=404{t.Fatal("missing session accepted",w.Code)}}
 if w:=f.request("DELETE",session,"");w.Code!=204{t.Fatal(w.Code)}
 if w:=f.request("POST",session,body);w.Code!=404{t.Fatal("deleted session accepted",w.Code)}
 if f.executor.calls.Load()!=0{t.Fatal("unauthorized call reached executor")}
}
func TestSessionCapacityAndReclamation(t *testing.T) {
 f:=newProtocolFixture(t,false);sessions:=make([]string,8)
 for i:=range sessions{sessions[i]=f.initialize(t)}
 body:=`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
 requireRPCCode(t,f.request("POST","",body),-32000)
 if w:=f.request("DELETE",sessions[0],"");w.Code!=204{t.Fatal(w.Code)}
 replacement:=f.initialize(t)
 if replacement==sessions[0]{t.Fatal("reused deleted session identifier")}
 if w:=f.request("POST",sessions[0],`{"jsonrpc":"2.0","id":2,"method":"ping"}`);w.Code!=404{t.Fatal("deleted session remained active")}
}
func TestStrictProtocolParams(t *testing.T) {
 f:=newProtocolFixture(t,false);session:=f.initialize(t)
 for _,body:=range []string{
  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"unexpected":true}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"unsupported"}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":null}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`,
  `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"unexpected":true}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{},"agentId":"forged"}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":null}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":[]}}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{},"_meta":null}}`,
 }{requireRPCCode(t,f.request("POST",session,body),-32602)}
 for _,body:=range []string{
  `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"unexpected":true}}`,
  `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"unexpected":true}}`,
  `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":null}}`,
 }{if w:=f.request("POST",session,body);w.Code!=400{t.Fatal("malformed notification accepted",w.Code,w.Body.String())}}
 for _,body:=range []string{
  `{"jsonrpc":"2.0","id":1,"method":"tools/list","extra":true}`,
  `{"jsonrpc":"2.0","id":1,"method":"tools/list"} {}`,
  "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"\xff\"}",
  strings.Repeat(" ",1024*1024)+`{}`,
 }{requireRPCCode(t,f.request("POST",session,body),-32700)}
 if f.executor.calls.Load()!=0{t.Fatal("malformed request reached executor")}
}
func TestSessionOwnedCancellationAndTypedIDs(t *testing.T) {
 f:=newProtocolFixture(t,true);owner:=f.initialize(t);other:=f.initialize(t)
 ownerDone:=startCall(f,owner,`7`);ownerCall:=takeStarted(t,f.executor)
 otherDone:=startCall(f,other,`7`);otherCall:=takeStarted(t,f.executor)
 if ownerCall.ID==otherCall.ID{t.Fatal("sessions share native call identity")}
 if w:=f.request("POST","wrong-session",`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`);w.Code!=404{t.Fatal("unknown session cancelled work",w.Code)}
 if w:=f.request("POST",other,`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`);w.Code!=202{t.Fatal(w.Code)}
 if id:=takeFinished(t,f.executor);id!=otherCall.ID{t.Fatal("cancelled another session's call",id)}
 requireRPCCode(t,takeResponse(t,otherDone),0)
 select{case <-ownerDone:t.Fatal("other session cancelled owner");default:}
 // A string and numeric JSON-RPC identifier are distinct within a session.
 stringDone:=startCall(f,owner,`"7"`);stringCall:=takeStarted(t,f.executor)
 if stringCall.ID==ownerCall.ID{t.Fatal("string and numeric IDs collide")}
 requireRPCCode(t,f.request("POST",owner,`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{}}}`),-32600)
 if w:=f.request("POST",owner,`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"7"}}`);w.Code!=202{t.Fatal(w.Code)}
 if id:=takeFinished(t,f.executor);id!=stringCall.ID{t.Fatal("typed cancellation crossed request IDs",id)}
 requireRPCCode(t,takeResponse(t,stringDone),0)
 if w:=f.request("DELETE",other,"");w.Code!=204{t.Fatal(w.Code)}
 select{case <-ownerDone:t.Fatal("other session deletion cancelled owner");default:}
 if w:=f.request("DELETE",owner,"");w.Code!=204{t.Fatal(w.Code)}
 if id:=takeFinished(t,f.executor);id!=ownerCall.ID{t.Fatal("owner deletion failed to cancel call",id)}
 requireRPCCode(t,takeResponse(t,ownerDone),0)
}
func TestCallCapacityAndCloseCancellation(t *testing.T) {
 f:=newProtocolFixture(t,true);session:=f.initialize(t)
 done:=make([]<-chan *httptest.ResponseRecorder,32)
 for i:=range done{done[i]=startCall(f,session,fmt.Sprint(i+1));takeStarted(t,f.executor)}
 requireRPCCode(t,f.request("POST",session,`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"meshra_workspace_read","arguments":{}}}`),-32000)
 if f.executor.calls.Load()!=32{t.Fatal("capacity rejection dispatched work")}
 f.handler.Close()
 for _,response:=range done{takeFinished(t,f.executor);requireRPCCode(t,takeResponse(t,response),0)}
 for _,body:=range []string{
  `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
  `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
 }{if w:=f.request("POST",session,body);w.Code!=404{t.Fatal("closed handler accepted request",w.Code,w.Body.String())}}
}
