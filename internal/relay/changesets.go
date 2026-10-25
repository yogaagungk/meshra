package relay

import (
 "context"
 "database/sql"
 "encoding/json"
 "errors"
 "fmt"
 "os"
 "path/filepath"
 "strings"
 "time"

 "meshra/internal/model"
 "meshra/internal/sandbox"
)

type workspaceChangeset struct {
 ID string `json:"id"`
 InvocationID string `json:"invocationId"`
 WorkspaceID string `json:"workspaceId"`
 ManifestHash string `json:"manifestHash"`
 Revision int `json:"revision"`
 State string `json:"state"`
 Changes []sandbox.Change `json:"changes"`
 Preview string `json:"preview"`
 PreviewTruncated bool `json:"previewTruncated"`
 CreatedAt time.Time `json:"createdAt"`
 UpdatedAt time.Time `json:"updatedAt"`
 Error string `json:"error,omitempty"`
}
// Private storage locations are derived from opaque IDs, never persisted in DTOs.
func(r *Relay)changesetDirectory(id string)string{return filepath.Join(r.sandboxStore(),"changeset-"+id)}
func(r *Relay)persistChangeset(ctx context.Context,invocationID,workspaceID string,m *sandbox.Manifest)(v workspaceChangeset,err error){
 if e:=ctx.Err();e!=nil{return v,e};if m==nil{return v,model.Err(400,"missing exact changeset manifest")}
 r.mu.Lock();defer r.mu.Unlock();if r.closed{return v,model.Err(503,"relay is stopping")};if _,err=r.workspace(workspaceID);err!=nil{return v,err}
 var count int;if err=r.db.QueryRow("SELECT count(*) FROM tool_invocations WHERE id=?",invocationID).Scan(&count);err!=nil{return v,err};if count!=1{return v,model.Err(404,"tool invocation not found")}
 if err=m.Save();err!=nil{return v,changesetFailure(err,m.Directory())}
 v=workspaceChangeset{ID:randomID(),InvocationID:invocationID,WorkspaceID:workspaceID,ManifestHash:m.Hash,Revision:1,State:"staged",Changes:m.Changes,Preview:redact(m.Preview),PreviewTruncated:m.PreviewTruncated,CreatedAt:now(),UpdatedAt:now()}
 dir:=r.changesetDirectory(v.ID);if err=os.Rename(m.Directory(),dir);err!=nil{return v,err};defer func(){if err!=nil{os.RemoveAll(dir)}}()
 body,err:=json.Marshal(v);if err!=nil{return v,err};tx,err:=r.db.Begin();if err!=nil{return v,err};defer tx.Rollback()
 if _,err=tx.Exec("INSERT INTO workspace_changesets(id,invocation_id,manifest_hash,state,body) VALUES(?,?,?,?,?)",v.ID,invocationID,m.Hash,v.State,string(body));err!=nil{return v,err}
 if err=insertAutomationEvent(tx,"changeset.staged",v.ID,"",map[string]any{"workspaceId":workspaceID,"manifestHash":m.Hash});err!=nil{return v,err};err=tx.Commit();return v,err
}
func(r *Relay)loadChangeset(id string)(workspaceChangeset,error){var v workspaceChangeset;var body string;e:=r.db.QueryRow("SELECT body FROM workspace_changesets WHERE id=?",id).Scan(&body);if e==sql.ErrNoRows{return v,model.Err(404,"changeset not found")};if e!=nil{return v,e};e=json.Unmarshal([]byte(body),&v);return v,e}
func(r *Relay)storeChangeset(v workspaceChangeset)error{b,e:=json.Marshal(v);if e!=nil{return e};tx,e:=r.db.Begin();if e!=nil{return e};defer tx.Rollback();if _,e=tx.Exec("UPDATE workspace_changesets SET state=?,revision=?,body=? WHERE id=?",v.State,v.Revision,string(b),v.ID);e!=nil{return e};if e=insertAutomationEvent(tx,"changeset."+v.State,v.ID,"",map[string]any{"revision":v.Revision,"manifestHash":v.ManifestHash});e!=nil{return e};return tx.Commit()}
func(r *Relay)changesetOperation(ctx context.Context,p model.Principal,op string,raw json.RawMessage)(any,error){
 if e:=r.requireConsole(p);e!=nil{return nil,e};if op=="changesets.get"||op=="changesets.list"{return r.changesetQuery(op,raw)};if op!="changesets.apply"{return nil,model.Err(404,"unknown changeset operation")}
 var q struct{ID string `json:"id"`;ManifestHash string `json:"manifestHash"`;ExpectedRevision int `json:"expectedRevision"`};if e:=decodeAutomation(raw,&q);e!=nil{return nil,e}
 return r.applyChangeset(ctx,p,q.ID,q.ManifestHash,q.ExpectedRevision,nil)
}
func(r *Relay)applyChangeset(ctx context.Context,p model.Principal,id,hash string,revision int,claim func()error)(workspaceChangeset,error){
 var v workspaceChangeset;if e:=r.requireConsole(p);e!=nil{return v,e}
 r.lifecycle.Lock();defer r.lifecycle.Unlock()
 r.mu.Lock();v,e:=r.loadChangeset(id);if e!=nil{r.mu.Unlock();return v,e};if v.Revision!=revision||v.ManifestHash!=hash||v.State!="staged"{r.mu.Unlock();return v,model.Err(409,"changeset is stale, already claimed, or previously interrupted; never replay it")};w,e:=r.workspace(v.WorkspaceID);r.mu.Unlock();if e!=nil{return v,e}
 m,e:=sandbox.LoadManifest(r.changesetDirectory(id),hash);if e!=nil{return v,changesetFailure(e,r.changesetDirectory(id))}
 claimed:=false;mutations:=map[string]string{}
 check:=func()error{if e:=ctx.Err();e!=nil{return e};if e:=r.requireConsole(p);e!=nil{return e};if claim!=nil{if e:=claim();e!=nil{return e}};if !claimed{r.mu.Lock();defer r.mu.Unlock();current,e:=r.loadChangeset(id);if e!=nil{return e};if current.Revision!=revision||current.ManifestHash!=hash||current.State!="staged"{return model.Err(409,"changeset was already claimed")};v.State="applying";v.Revision++;v.UpdatedAt=now();if e=r.storeChangeset(v);e!=nil{return e};claimed=true};return nil}
 journal:=func(record sandbox.ApplyRecord)error{
  r.mu.Lock();defer r.mu.Unlock();record.Error=changesetSafeText(record.Error,r.changesetDirectory(id));_,e:=r.db.Exec("INSERT INTO workspace_apply_journal(changeset_id,path,state,original_hash,result_hash,error,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(changeset_id,path) DO UPDATE SET state=excluded.state,original_hash=excluded.original_hash,result_hash=excluded.result_hash,error=excluded.error,updated_at=excluded.updated_at",id,record.Path,record.State,record.OriginalHash,record.ResultHash,record.Error,now().Format(time.RFC3339Nano));if e==nil{mutations[record.Path]=record.State};return e
 }
 e=m.Apply(ctx,w.Path,check,journal)
 if !claimed{return v,changesetFailure(e,r.changesetDirectory(id))}
 v.UpdatedAt=now();v.Revision++;v.State="applied";if e!=nil{v.State="partial";v.Error=changesetSafeText(e.Error(),r.changesetDirectory(id))}
 r.mu.Lock();persistErr:=r.storeChangeset(v);r.mu.Unlock();if persistErr!=nil{return v,persistErr}
 if e!=nil{
  var changed,uncertain []string
  for _,c:=range m.Changes{
   switch mutations[c.Path]{
   case "applied":changed=append(changed,c.Path)
   case "created","created_mode_uncertain":changed=append(changed,c.Path);uncertain=append(uncertain,c.Path)
   case "applying","uncertain","pending_mode","mode_uncertain":uncertain=append(uncertain,c.Path)
   }
  }
  status:=503
  var apiErr *model.Error
  if errors.As(sandboxError(e),&apiErr){status=apiErr.Status}
  return v,model.Err(status,fmt.Sprintf("changeset partially applied; changed=[%s]; uncertain=[%s]; no rollback or replay: %s",strings.Join(changed,", "),strings.Join(uncertain,", "),changesetSafeText(e.Error(),r.changesetDirectory(id))))
 }
 return v,nil
}

// pruneChangesets runs under relay ownership. It never removes a referenced
// active invocation or a nonterminal/uncertain application journal.
func(r *Relay)pruneChangesets(before time.Time)error{
 rows,e:=r.db.Query("SELECT c.id,c.body FROM workspace_changesets c JOIN tool_invocations i ON i.id=c.invocation_id JOIN tasks t ON t.id=i.task_id WHERE c.state IN ('applied','staged') AND i.state IN ('completed','denied','failed','cancelled') AND t.state IN ('completed','failed','cancelled','timed_out')");if e!=nil{return e};var ids []string;for rows.Next(){var id,body string;if e=rows.Scan(&id,&body);e!=nil{rows.Close();return e};var v workspaceChangeset;if e=json.Unmarshal([]byte(body),&v);e!=nil{rows.Close();return e};if v.UpdatedAt.Before(before){ids=append(ids,id)}};e=rows.Err();rows.Close();if e!=nil{return e}
 for _,id:=range ids{tx,e:=r.db.Begin();if e!=nil{return e};if _,e=tx.Exec("DELETE FROM workspace_apply_journal WHERE changeset_id=?",id);e==nil{_,e=tx.Exec("DELETE FROM workspace_changesets WHERE id=?",id)};if e!=nil{tx.Rollback();return e};if e=tx.Commit();e!=nil{return e};if e=os.RemoveAll(r.changesetDirectory(id));e!=nil{return e}};return nil
}

func changesetSafeText(message,privateDir string)string{return strings.ReplaceAll(redact(message),privateDir,"[private changeset]")}
func changesetFailure(e error,privateDir string)error{
 if e==nil||errors.Is(e,context.Canceled)||errors.Is(e,context.DeadlineExceeded){return e}
 status:=500
 var apiErr *model.Error
 if errors.As(sandboxError(e),&apiErr){status=apiErr.Status}else if errors.Is(e,os.ErrNotExist){status=503}
 return model.Err(status,changesetSafeText(e.Error(),privateDir))
}
