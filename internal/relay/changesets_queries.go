package relay

import (
 "encoding/json"
 "time"

 "meshra/internal/model"
)

type workspaceApplyEntry struct {
 Path string `json:"path"`
 State string `json:"state"`
 OriginalHash string `json:"originalHash"`
 ResultHash string `json:"resultHash"`
 Error string `json:"error,omitempty"`
 UpdatedAt string `json:"updatedAt"`
}
type workspaceChangesetDetail struct {
 workspaceChangeset
 Journal []workspaceApplyEntry `json:"journal"`
}
type workspaceChangesetSummary struct {
 ID string `json:"id"`
 InvocationID string `json:"invocationId"`
 WorkspaceID string `json:"workspaceId"`
 ManifestHash string `json:"manifestHash"`
 Revision int `json:"revision"`
 State string `json:"state"`
 CreatedAt time.Time `json:"createdAt"`
 UpdatedAt time.Time `json:"updatedAt"`
 Error string `json:"error,omitempty"`
}

// The caller has validated live console provenance before acquiring r.mu.
func(r *Relay)changesetQuery(op string,raw json.RawMessage)(any,error){
 switch op{
 case "changesets.get":
  var q struct{ID string `json:"id"`};if e:=decodeAutomation(raw,&q);e!=nil{return nil,e}
  r.mu.Lock();defer r.mu.Unlock();v,e:=r.loadChangeset(q.ID);if e!=nil{return nil,e}
  rows,e:=r.db.Query("SELECT path,state,original_hash,result_hash,error,updated_at FROM workspace_apply_journal WHERE changeset_id=? ORDER BY path LIMIT 100001",q.ID);if e!=nil{return nil,e};defer rows.Close()
  entries:=[]workspaceApplyEntry{};for rows.Next(){if len(entries)>=100000{return nil,model.Err(413,"changeset journal exceeds configured maximum")};var entry workspaceApplyEntry;if e=rows.Scan(&entry.Path,&entry.State,&entry.OriginalHash,&entry.ResultHash,&entry.Error,&entry.UpdatedAt);e!=nil{return nil,e};entry.Error=redact(entry.Error);entries=append(entries,entry)};if e=rows.Err();e!=nil{return nil,e};v.Error=redact(v.Error);v.Preview=redact(v.Preview);return workspaceChangesetDetail{workspaceChangeset:v,Journal:entries},nil
 case "changesets.list":
  var q struct{TaskID string `json:"taskId"`;After string `json:"after"`;Limit int `json:"limit"`};if e:=decodeAutomation(raw,&q);e!=nil{return nil,e};if q.Limit==0{q.Limit=50};if q.Limit<1||q.Limit>100||len(q.After)>64||len(q.TaskID)>64{return nil,model.Err(400,"invalid changeset page")}
  r.mu.Lock();defer r.mu.Unlock();if q.TaskID!=""{var n int;if e:=r.db.QueryRow("SELECT count(*) FROM tasks WHERE id=?",q.TaskID).Scan(&n);e!=nil{return nil,e};if n==0{return nil,model.Err(404,"task not found")}}
  // Extract only summaries in SQLite: list never materializes large manifests,
  // binary replacement previews or private blob paths.
  rows,e:=r.db.Query("SELECT c.id,c.invocation_id,c.manifest_hash,c.revision,c.state,json_object('workspaceId',json_extract(c.body,'$.workspaceId'),'createdAt',json_extract(c.body,'$.createdAt'),'updatedAt',json_extract(c.body,'$.updatedAt'),'error',coalesce(json_extract(c.body,'$.error'),'')) FROM workspace_changesets c JOIN tool_invocations i ON i.id=c.invocation_id WHERE (?='' OR i.task_id=?) AND (?='' OR c.id>?) ORDER BY c.id LIMIT ?",q.TaskID,q.TaskID,q.After,q.After,q.Limit+1);if e!=nil{return nil,e};defer rows.Close()
  values:=[]workspaceChangesetSummary{};more:=false;for rows.Next(){if len(values)==q.Limit{more=true;break};var value workspaceChangesetSummary;var metadata string;if e=rows.Scan(&value.ID,&value.InvocationID,&value.ManifestHash,&value.Revision,&value.State,&metadata);e!=nil{return nil,e};if e=json.Unmarshal([]byte(metadata),&value);e!=nil{return nil,e};value.Error=redact(value.Error);values=append(values,value)};if e=rows.Err();e!=nil{return nil,e};after:="";if more&&len(values)>0{after=values[len(values)-1].ID};return struct{Changesets []workspaceChangesetSummary `json:"changesets"`;HasMore bool `json:"hasMore"`;NextAfter string `json:"nextAfter,omitempty"`}{values,more,after},nil
 }
 return nil,model.Err(404,"unknown changeset query")
}
