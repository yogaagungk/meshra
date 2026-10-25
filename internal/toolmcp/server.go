// Package toolmcp serves only the tool catalog of an already-authenticated attempt.
package toolmcp

import (
 "bytes"
 "context"
 "crypto/rand"
 "encoding/hex"
 "encoding/json"
 "io"
 "net/http"
 "strconv"
 "sync"
 "unicode/utf8"

 "meshra/internal/model"
)

const maxFrame = 1024*1024
const maxSessions = 8
const maxCalls = 32

type request struct{JSONRPC string `json:"jsonrpc"`;ID json.RawMessage `json:"id,omitempty"`;Method string `json:"method"`;Params json.RawMessage `json:"params,omitempty"`}
type session struct{calls map[string]context.CancelFunc}
type Handler struct{executor model.ToolExecutor;authorize func(context.Context,*http.Request)error;mu sync.Mutex;sessions map[string]*session;closed bool;calls int}
func New(executor model.ToolExecutor,authorize func(context.Context,*http.Request)error)*Handler{return &Handler{executor:executor,authorize:authorize,sessions:make(map[string]*session)}}
func rpc(w http.ResponseWriter,id json.RawMessage,result any,code int,message string){
 response:=map[string]any{"jsonrpc":"2.0","id":id};if code!=0{response["error"]=map[string]any{"code":code,"message":message}}else{response["result"]=result}
 data,err:=json.Marshal(response);if err!=nil||len(data)>maxFrame{data,_=json.Marshal(map[string]any{"jsonrpc":"2.0","id":id,"error":map[string]any{"code":-32603,"message":"tool response exceeds bounded frame"}})}
 w.Header().Set("Content-Type","application/json");w.WriteHeader(200);_,_=w.Write(data)
}
func opaqueID()string{var b [16]byte;if _,err:=rand.Read(b[:]);err!=nil{panic(err)};return hex.EncodeToString(b[:])}
func protocol(version string)bool{switch version{case "2024-11-05","2025-03-26","2025-06-18","2025-11-25":return true};return false}
func nativeID(raw json.RawMessage)(string,bool){var s string;if json.Unmarshal(raw,&s)==nil{return "s:"+s,s!=""&&len(s)<=128};var n json.Number;if json.Unmarshal(raw,&n)==nil{if _,err:=strconv.ParseInt(string(n),10,64);err==nil{return "n:"+string(n),true}};return "",false}
func decode(raw json.RawMessage,dst any)error{if len(raw)==0{raw=json.RawMessage(`{}`)};return strict(raw,dst)}
func strict(raw json.RawMessage,dst any)error{
 raw=bytes.TrimSpace(raw);if !utf8.Valid(raw)||len(raw)==0||raw[0]!='{'{return model.Err(400,"one UTF-8 object required")}
 decoder:=json.NewDecoder(bytes.NewReader(raw));decoder.DisallowUnknownFields();if err:=decoder.Decode(dst);err!=nil{return err};if err:=decoder.Decode(new(any));err!=io.EOF{return model.Err(400,"one object required")};return nil
}

func(h *Handler)ServeHTTP(w http.ResponseWriter,r *http.Request){
 w.Header().Set("Cache-Control","no-store");w.Header().Set("X-Content-Type-Options","nosniff")
 if r.URL.Path!="/mcp"||r.URL.RawQuery!=""{http.Error(w,"not found",404);return}
 if _,present:=r.Header["Origin"];present{http.Error(w,"browser origins are forbidden",403);return}
 if h.executor==nil{http.Error(w,"active executor unavailable",503);return}
 if h.authorize==nil{http.Error(w,"attempt authorization unavailable",503);return}
 if err:=h.authorize(r.Context(),r);err!=nil{status:=403;if e,ok:=err.(*model.Error);ok{status=e.Status};http.Error(w,"attempt credential is not active",status);return}
 h.mu.Lock();closed:=h.closed;h.mu.Unlock();if closed{http.Error(w,"MCP attempt closed",404);return}
 sessionID:=r.Header.Get("Mcp-Session-Id")
 if r.Method==http.MethodDelete{
  h.mu.Lock();s:=h.sessions[sessionID];delete(h.sessions,sessionID);if s!=nil{for _,cancel:=range s.calls{cancel()}};h.mu.Unlock();if s==nil{http.Error(w,"MCP attempt session expired",404);return};w.WriteHeader(204);return
 }
 if r.Method!=http.MethodPost{w.Header().Set("Allow","POST, DELETE");http.Error(w,"method not allowed",405);return}
 raw,err:=io.ReadAll(http.MaxBytesReader(w,r.Body,maxFrame));var q request
 if err!=nil||strict(raw,&q)!=nil{rpc(w,nil,nil,-32700,"invalid bounded JSON-RPC request");return}
 if q.JSONRPC!="2.0"||q.Method==""{rpc(w,q.ID,nil,-32600,"invalid JSON-RPC request");return}
 if len(q.ID)>0{if _,ok:=nativeID(q.ID);!ok{rpc(w,nil,nil,-32600,"invalid JSON-RPC identifier");return}}
 if q.Method=="initialize"{
  var p struct{ProtocolVersion string `json:"protocolVersion"`;Capabilities json.RawMessage `json:"capabilities"`;ClientInfo json.RawMessage `json:"clientInfo"`}
  if _,ok:=nativeID(q.ID);!ok||sessionID!=""||decode(q.Params,&p)!=nil||!protocol(p.ProtocolVersion){rpc(w,q.ID,nil,-32602,"unsupported MCP initialization");return}
  for _,value:=range []json.RawMessage{p.Capabilities,p.ClientInfo}{if len(value)>0&&strict(value,new(map[string]any))!=nil{rpc(w,q.ID,nil,-32602,"invalid MCP client metadata");return}}
  h.mu.Lock();if h.closed{h.mu.Unlock();http.Error(w,"MCP attempt closed",404);return};if len(h.sessions)>=maxSessions{h.mu.Unlock();rpc(w,q.ID,nil,-32000,"attempt MCP session capacity reached");return};id:=opaqueID();h.sessions[id]=&session{calls:make(map[string]context.CancelFunc)};h.mu.Unlock()
  w.Header().Set("Mcp-Session-Id",id);rpc(w,q.ID,map[string]any{"protocolVersion":p.ProtocolVersion,"capabilities":map[string]any{"tools":map[string]bool{"listChanged":false}},"serverInfo":map[string]string{"name":"meshra-active-tools","version":"1"}},0,"");return
 }
 h.mu.Lock();s:=h.sessions[sessionID];h.mu.Unlock()
 if s==nil{http.Error(w,"MCP attempt session expired",404);return}
 if q.Method=="notifications/initialized"{if len(q.ID)>0||decode(q.Params,new(struct{}))!=nil{w.WriteHeader(400);return};w.WriteHeader(202);return}
 if q.Method=="notifications/cancelled"{
  var p struct{RequestID json.RawMessage `json:"requestId"`;Reason string `json:"reason,omitempty"`}
  if len(q.ID)>0||decode(q.Params,&p)!=nil{w.WriteHeader(400);return};id,ok:=nativeID(p.RequestID);if !ok{w.WriteHeader(400);return}
  h.mu.Lock();cancel:=s.calls[id];h.mu.Unlock();if cancel!=nil{cancel()};w.WriteHeader(202);return
 }
 if _,ok:=nativeID(q.ID);!ok{rpc(w,nil,nil,-32600,"request identifier required");return}
 switch q.Method{
 case "ping":if decode(q.Params,new(struct{}))!=nil{rpc(w,q.ID,nil,-32602,"invalid ping params");return};rpc(w,q.ID,map[string]any{},0,"")
 case "tools/list":
  var p struct{Cursor string `json:"cursor,omitempty"`};if decode(q.Params,&p)!=nil||p.Cursor!=""{rpc(w,q.ID,nil,-32602,"unsupported tool cursor");return}
  tools:=h.executor.Definitions();if tools==nil{tools=[]model.ToolDefinition{}};rpc(w,q.ID,map[string]any{"tools":tools},0,"")
 case "tools/call":
  var p struct{Name string `json:"name"`;Arguments json.RawMessage `json:"arguments"`;Meta json.RawMessage `json:"_meta,omitempty"`}
  id,ok:=nativeID(q.ID);if !ok||decode(q.Params,&p)!=nil||(len(p.Arguments)>0&&strict(p.Arguments,new(map[string]any))!=nil)||(len(p.Meta)>0&&strict(p.Meta,new(map[string]any))!=nil){rpc(w,q.ID,nil,-32602,"invalid tool call");return}
  allowed:=false;for _,definition:=range h.executor.Definitions(){if definition.Name==p.Name{allowed=true;break}};if !allowed{rpc(w,q.ID,nil,-32602,"tool is not in the active catalog");return}
  child,cancel:=context.WithCancel(r.Context());defer cancel()
  h.mu.Lock();if h.closed||h.sessions[sessionID]!=s{h.mu.Unlock();http.Error(w,"MCP attempt session expired",404);return};if s.calls[id]!=nil{h.mu.Unlock();rpc(w,q.ID,nil,-32600,"RPC call already pending");return};if h.calls>=maxCalls{h.mu.Unlock();rpc(w,q.ID,nil,-32000,"attempt MCP call capacity reached");return};s.calls[id]=cancel;h.calls++;h.mu.Unlock();defer func(){h.mu.Lock();delete(s.calls,id);h.calls--;h.mu.Unlock()}()
  if child.Err()!=nil{rpc(w,q.ID,nil,-32603,"tool request cancelled before dispatch");return}
  result,err:=h.executor.Call(child,model.ToolCall{ID:sessionID+":"+id,Name:p.Name,Arguments:p.Arguments})
  if err!=nil{result=model.ToolResult{IsError:true,Content:[]model.ToolContent{{Type:"text",Text:"Tool call denied or failed"}}};if e,ok:=err.(*model.Error);ok{result.Content[0].Text=e.Message}}
  rpc(w,q.ID,result,0,"")
 default:rpc(w,q.ID,nil,-32601,"only initialize, tools and cancellation are supported")
 }
}
func(h *Handler)Close(){h.mu.Lock();defer h.mu.Unlock();h.closed=true;for id,s:=range h.sessions{for _,cancel:=range s.calls{cancel()};delete(h.sessions,id)}}
