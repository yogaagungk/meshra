package relay

import (
 "context"
 "database/sql"
 "encoding/json"
 "errors"
 "os"
 "path/filepath"

 "meshra/internal/model"
 "meshra/internal/sandbox"
)

type runtimeSettings struct {
 Revision int `json:"revision"`
 sandbox.Config
}
func(r *Relay)sandboxStore()string{return filepath.Join(filepath.Dir(r.lock.Name()),"sandbox")}
func(r *Relay)runtimeConfigLocked()(sandbox.Config,int,error){var body string;var rev int;e:=r.db.QueryRow("SELECT revision,body FROM automation_runtime_settings WHERE id=1").Scan(&rev,&body);if e==sql.ErrNoRows{return sandbox.Config{},0,nil};if e!=nil{return sandbox.Config{},0,e};var c sandbox.Config;e=json.Unmarshal([]byte(body),&c);return c,rev,e}
func(r *Relay)runtimeConfig()(sandbox.Config,int,error){r.mu.Lock();defer r.mu.Unlock();return r.runtimeConfigLocked()}
func(r *Relay)runtimeOperation(ctx context.Context,p model.Principal,op string,raw json.RawMessage)(any,error){
 if e:=requireAdmin(p);e!=nil{return nil,e}
 switch op{
 case "runtime.status":
  var q struct{};if e:=decodeAutomation(raw,&q);e!=nil{return nil,e};c,rev,e:=r.runtimeConfig();if e!=nil{return nil,e};return struct{Revision int `json:"revision"`;Config sandbox.Config `json:"config"`;Status sandbox.Status `json:"status"`}{rev,c,sandbox.Check(ctx,c)},nil
 case "runtime.configure":
  var q struct{ExpectedRevision int `json:"expectedRevision"`;Executables map[string]string `json:"executables"`;RuntimePaths []string `json:"runtimePaths"`;Limits sandbox.Limits `json:"limits"`};if e:=decodeAutomation(raw,&q);e!=nil{return nil,e}
  c,e:=sandbox.Configure(sandbox.Config{Executables:q.Executables,RuntimePaths:q.RuntimePaths,Limits:q.Limits})
  if e!=nil{
   if errors.Is(e,os.ErrNotExist){return nil,model.Err(503,"selected runtime file is missing: "+e.Error())}
   if errors.Is(e,sandbox.ErrDenied)||errors.Is(e,sandbox.ErrLimit)||errors.Is(e,sandbox.ErrUnavailable)||errors.Is(e,os.ErrPermission){return nil,sandboxError(e)}
   return nil,model.Err(400,e.Error())
  }
  r.lifecycle.Lock();defer r.lifecycle.Unlock();r.mu.Lock();defer r.mu.Unlock();if r.closed{return nil,model.Err(503,"relay is stopping")};_,rev,e:=r.runtimeConfigLocked();if e!=nil{return nil,e};if rev!=q.ExpectedRevision{return nil,model.Err(409,"runtime revision changed")}
  // Configuration never changes the manifest beneath a worker in progress.
  if sandbox.ActiveWorkers()!=0{return nil,model.Err(409,"confined workers are active")}
  b,e:=json.Marshal(c);if e!=nil{return nil,e};tx,e:=r.db.Begin();if e!=nil{return nil,e};defer tx.Rollback();rev++
  if _,e=tx.Exec("INSERT INTO automation_runtime_settings(id,revision,body) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,body=excluded.body",rev,string(b));e!=nil{return nil,e}
  if e=insertAutomationEvent(tx,"runtime.configured","runtime","",map[string]any{"revision":rev});e!=nil{return nil,e};if e=tx.Commit();e!=nil{return nil,e};return runtimeSettings{Revision:rev,Config:c},nil
 };return nil,model.Err(404,"unknown runtime operation")
}
func(r *Relay)prepareWorkspace(ctx context.Context,workspaceID string)(*sandbox.Snapshot,error){
 r.mu.Lock();w,e:=r.workspace(workspaceID);if e!=nil{r.mu.Unlock();return nil,e};c,_,e:=r.runtimeConfigLocked();r.mu.Unlock();if e!=nil{return nil,e};s,e:=sandbox.SnapshotWorkspace(ctx,w.Path,r.sandboxStore(),c.Limits);return s,sandboxError(e)
}
func sandboxError(e error)error{if e==nil{return nil};switch{case errors.Is(e,sandbox.ErrUnavailable):return model.Err(503,e.Error());case errors.Is(e,sandbox.ErrConflict):return model.Err(409,e.Error());case errors.Is(e,sandbox.ErrLimit):return model.Err(413,e.Error());case errors.Is(e,sandbox.ErrDenied),errors.Is(e,os.ErrPermission):return model.Err(403,e.Error())};return e}

// cleanupSandboxStore is a startup-only hook, before any prepared worker exists.
func(r *Relay)cleanupSandboxStore()error{
 rows,e:=r.db.Query("SELECT id FROM workspace_changesets")
 if e!=nil{return e}
 retained:=map[string]bool{}
 for rows.Next(){
  var id string
  if e=rows.Scan(&id);e!=nil{rows.Close();return e}
  retained[id]=true
 }
 e=rows.Err();rows.Close()
 if e!=nil{return e}
 rows,e=r.db.Query("SELECT body FROM workspace_changesets WHERE state='applying'")
 if e!=nil{return e}
 interrupted:=[]workspaceChangeset{}
 for rows.Next(){
  var body string
  if e=rows.Scan(&body);e!=nil{rows.Close();return e}
  var v workspaceChangeset
  if e=json.Unmarshal([]byte(body),&v);e!=nil{rows.Close();return e}
  interrupted=append(interrupted,v)
 }
 e=rows.Err();rows.Close()
 if e!=nil{return e}
 for _,v:=range interrupted{
  v.State="uncertain"
  v.Revision++
  v.UpdatedAt=now()
  v.Error="Workspace application was interrupted; inspect the per-path journal. No rollback, automatic resume, or replay."
  if e=r.storeChangeset(v);e!=nil{return e}
 }
 return sandbox.CleanupStore(r.sandboxStore(),retained)
}
