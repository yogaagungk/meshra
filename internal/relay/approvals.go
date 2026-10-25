package relay

import (
 "context"
 "database/sql"
 "encoding/json"
 "errors"
 "time"

 "meshra/internal/model"
)

type toolApproval struct {
 ID string `json:"id"`
 InvocationID string `json:"invocationId"`
 Kind string `json:"kind"`
 Revision int `json:"revision"`
 Status string `json:"status"`
 ExpiresAt time.Time `json:"expiresAt"`
 Owner string `json:"owner,omitempty"`
 Decision string `json:"decision,omitempty"`
 DecisionKey string `json:"decisionKey,omitempty"`
 DecisionRevision int `json:"decisionRevision,omitempty"`
 AnswerHash string `json:"answerHash,omitempty"`
}
type approvalView struct {
 Type string `json:"type"`
 ID string `json:"id"`
 InvocationID string `json:"invocationId"`
 TaskID string `json:"taskId"`
 ProfileID string `json:"profileId"`
 Name string `json:"name"`
 Kind string `json:"kind"`
 Revision int `json:"revision"`
 Status string `json:"status"`
 ExpiresAt time.Time `json:"expiresAt"`
 ActionHash string `json:"actionHash"`
 Action toolPreview `json:"action"`
}

func (r *Relay) loadToolApproval(id string)(toolApproval,toolInvocation,error){
 var a toolApproval;var v toolInvocation;var body,expires string
 err:=r.db.QueryRow("SELECT body,status,revision,expires_at FROM tool_approvals WHERE id=?",id).Scan(&body,&a.Status,&a.Revision,&expires)
 if errors.Is(err,sql.ErrNoRows){return a,v,model.Err(404,"approval not found")};if err!=nil{return a,v,err}
 status,revision:=a.Status,a.Revision
 if err=json.Unmarshal([]byte(body),&a);err!=nil{return a,v,err};a.Status=status;a.Revision=revision
 if a.ExpiresAt,err=time.Parse(time.RFC3339Nano,expires);err!=nil{return a,v,err}
 if err=r.db.QueryRow("SELECT body,state FROM tool_invocations WHERE id=?",a.InvocationID).Scan(&body,&v.State);err!=nil{return a,v,err}
 state:=v.State;if err=json.Unmarshal([]byte(body),&v);err!=nil{return a,v,err};v.State=state
 return a,v,nil
}
func makeApprovalView(a toolApproval,v toolInvocation)approvalView{return approvalView{Type:"attention",ID:a.ID,InvocationID:a.InvocationID,TaskID:v.TaskID,ProfileID:v.ProfileID,Name:v.Name,Kind:a.Kind,Revision:a.Revision,Status:a.Status,ExpiresAt:a.ExpiresAt,ActionHash:v.ActionHash,Action:v.Preview}}
func (r *Relay) approvalCallLocked(v toolInvocation)(*toolBroker,*brokerCall,error){
 b:=r.toolBrokers[v.TaskID]
 if b==nil||b.attempt!=v.Attempt{return nil,nil,model.Err(409,"approval attempt is no longer active")}
 call:=b.calls[v.CallID]
 if call==nil||call.invocation.ID!=v.ID||call.done{return nil,nil,model.Err(409,"approval invocation is no longer pending")}
 _,_,epoch,err:=b.activeLocked();if err!=nil{return nil,nil,err}
 if epoch!=v.Epoch||call.invocation.ActionHash!=v.ActionHash{return nil,nil,model.Err(409,"approval action is stale")}
 return b,call,nil
}
func signalToolCall(call *brokerCall){select{case call.wake<-struct{}{}:default:}}

func (b *toolBroker) awaitHuman(ctx context.Context,call *brokerCall)error{
 r:=b.relay
 owner:=b.snapshot.ConsoleSessionOwner
 if owner!=""{if err:=r.requireConsole(model.Principal{Admin:true,ConsoleSessionID:owner});err!=nil{return err}}
 r.mu.Lock()
 t,_,epoch,err:=b.activeLocked();if err!=nil{r.mu.Unlock();return err}
 if epoch!=call.invocation.Epoch{r.mu.Unlock();return model.Err(409,"tool context changed before approval")}
 expires:=now().Add(5*time.Minute);if t.Deadline.Before(expires){expires=t.Deadline}
 kind:="approval";if call.action.question{kind="question"}
 a:=toolApproval{ID:randomID(),InvocationID:call.invocation.ID,Kind:kind,Revision:1,Status:"pending",ExpiresAt:expires,Owner:owner}
 body,_:=json.Marshal(a)
 tx,err:=r.db.Begin();if err==nil{
  _,err=tx.Exec("INSERT INTO tool_approvals(id,invocation_id,revision,status,expires_at,body) VALUES(?,?,1,'pending',?,?)",a.ID,a.InvocationID,a.ExpiresAt.Format(time.RFC3339Nano),string(body))
  if err==nil&&call.action.question{_,err=tx.Exec("UPDATE tool_invocations SET state='waiting_question' WHERE id=? AND state='pending'",a.InvocationID)}
  if err==nil{err=insertAutomationEvent(tx,"attention.created",a.ID,call.invocation.ProfileID,map[string]string{"taskId":t.ID,"kind":kind})}
  if err==nil{err=tx.Commit()}else{tx.Rollback()}
 }
 if err==nil{call.approvalID=a.ID;call.owner=owner}
 r.mu.Unlock();if err!=nil{return err}
 timer:=time.NewTimer(time.Until(expires));defer timer.Stop()
 for{
  select{
  case <-ctx.Done():return ctx.Err()
  case <-timer.C:
   r.mu.Lock();r.expireApprovalLocked(a.ID);r.mu.Unlock();return model.Err(409,"human decision expired")
  case <-call.wake:
   r.mu.Lock();current,_,loadErr:=r.loadToolApproval(a.ID);r.mu.Unlock()
   if loadErr!=nil{return loadErr}
   switch current.Status{
   case "approved","answered":return nil
   case "denied":return model.Err(403,"human denied this exact action")
   case "expired","invalidated":return model.Err(409,"human decision is no longer valid")
   }
  }
 }
}

func (r *Relay) expireApprovalLocked(id string){
 a,v,err:=r.loadToolApproval(id);if err!=nil||a.Status!="pending"||a.ExpiresAt.After(now()){return}
 tx,err:=r.db.Begin();if err!=nil{return};defer tx.Rollback()
 if _,err=tx.Exec("UPDATE tool_approvals SET status='expired',revision=revision+1 WHERE id=? AND status='pending'",id);err!=nil{return}
 if _,err=tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE id=? AND state IN ('pending','waiting_question')",v.ID);err!=nil{return}
 if err=insertAutomationEvent(tx,"attention.expired",id,v.ProfileID,map[string]string{"taskId":v.TaskID});err!=nil{return}
 if err=tx.Commit();err==nil{if b:=r.toolBrokers[v.TaskID];b!=nil{if call:=b.calls[v.CallID];call!=nil{signalToolCall(call)}}}
}

func (r *Relay) approvalOperation(ctx context.Context,p model.Principal,op string,raw json.RawMessage)(any,error){
 if err:=r.requireConsole(p);err!=nil{return nil,err}
 if err:=ctx.Err();err!=nil{return nil,err}
 if op=="approvals.list"{
  var q struct{TaskID string `json:"taskId"`;ProfileID string `json:"profileId"`}
  if err:=decodeAutomation(raw,&q);err!=nil{return nil,err}
  r.mu.Lock();defer r.mu.Unlock()
  rows,err:=r.db.Query(`SELECT a.id FROM tool_approvals a JOIN tool_invocations i ON i.id=a.invocation_id WHERE (?='' OR i.task_id=?) AND (?='' OR json_extract(i.body,'$.profileId')=?) ORDER BY (a.status IN ('pending','approved','answered')) DESC,a.expires_at DESC LIMIT 100`,q.TaskID,q.TaskID,q.ProfileID,q.ProfileID)
  if err!=nil{return nil,err}
  ids:=[]string{};for rows.Next(){var id string;if err=rows.Scan(&id);err!=nil{break};ids=append(ids,id)};if err==nil{err=rows.Err()};rows.Close();if err!=nil{return nil,err}
  items:=make([]approvalView,0,len(ids));for _,id:=range ids{r.expireApprovalLocked(id);a,v,err:=r.loadToolApproval(id);if err!=nil{return nil,err};items=append(items,makeApprovalView(a,v))}
  return map[string]any{"items":items},nil
 }
 var id,decision,key,hash,answer string;var revision int
 if op=="approvals.decide"{
  var q struct{ID string `json:"id"`;ExpectedRevision int `json:"expectedRevision"`;Decision string `json:"decision"`;ActionHash string `json:"actionHash"`;IdempotencyKey string `json:"idempotencyKey"`}
  if err:=decodeAutomation(raw,&q);err!=nil{return nil,err}
  if q.Decision!="approve"&&q.Decision!="deny"{return nil,model.Err(400,"decision must be approve or deny")}
  if q.IdempotencyKey==""||len(q.IdempotencyKey)>200||!safeRegistryText(q.IdempotencyKey)||redact(q.IdempotencyKey)!=q.IdempotencyKey{return nil,model.Err(400,"invalid decision idempotencyKey")}
  id,revision,decision,key,hash=q.ID,q.ExpectedRevision,q.Decision,q.IdempotencyKey,q.ActionHash
 }else if op=="questions.answer"{
  var q struct{ID string `json:"id"`;ExpectedRevision int `json:"expectedRevision"`;Answer string `json:"answer"`}
  if err:=decodeAutomation(raw,&q);err!=nil{return nil,err}
  if q.Answer==""||len(q.Answer)>4096{return nil,model.Err(400,"answer must be 1..4096 bytes")}
  id,revision,answer=q.ID,q.ExpectedRevision,q.Answer;decision="answer"
 }else{return nil,model.Err(404,"unknown approval operation")}
 if id==""||revision<1{return nil,model.Err(400,"id and expectedRevision required")}
 // A second live-store validation follows parsing and immediately precedes the
 // authoritative decision transaction; callbacks never run while holding r.mu.
 if err:=r.requireConsole(p);err!=nil{return nil,err}
 r.mu.Lock();defer r.mu.Unlock()
 a,v,err:=r.loadToolApproval(id);if err!=nil{return nil,err}
 if a.Decision!=""{
  identical:=a.Decision==decision&&a.DecisionRevision==revision
  if decision=="answer"{identical=identical&&a.AnswerHash==tokenHash(answer)}else{identical=identical&&a.DecisionKey==key&&v.ActionHash==hash}
  if identical{return makeApprovalView(a,v),nil}
  return nil,model.Err(409,"decision conflicts with the recorded response")
 }
 if a.Revision!=revision||a.Status!="pending"||!a.ExpiresAt.After(now()){return nil,model.Err(409,"decision is stale or expired")}
 if (decision=="answer")!=(a.Kind=="question"){return nil,model.Err(400,"decision kind does not match this attention record")}
 if decision!="answer"&&hash!=v.ActionHash{return nil,model.Err(409,"action hash does not match reviewed action")}
 _,call,err:=r.approvalCallLocked(v);if err!=nil{return nil,err}
 a.Decision=decision;a.DecisionKey=key;a.DecisionRevision=revision;a.Owner=p.ConsoleSessionID;a.Revision++
 if decision=="answer"{a.AnswerHash=tokenHash(answer);a.Status="answered"}else if decision=="approve"{a.Status="approved"}else{a.Status="denied"}
 body,_:=json.Marshal(a)
 tx,err:=r.db.Begin();if err!=nil{return nil,err};defer tx.Rollback()
 updated,err:=tx.Exec("UPDATE tool_approvals SET status=?,revision=?,body=? WHERE id=? AND revision=? AND status='pending'",a.Status,a.Revision,string(body),id,revision);if err!=nil{return nil,err}
 count,_:=updated.RowsAffected();if count!=1{return nil,model.Err(409,"decision raced with invalidation")}
 invocationState:="approved";if decision=="deny"{invocationState="denied"}
 if _,err=tx.Exec("UPDATE tool_invocations SET state=? WHERE id=? AND state IN ('pending','waiting_question')",invocationState,v.ID);err!=nil{return nil,err}
 if err=insertAutomationEvent(tx,"attention."+a.Status,id,v.ProfileID,map[string]string{"taskId":v.TaskID});err!=nil{return nil,err}
 if err=tx.Commit();err!=nil{return nil,err}
 call.owner=p.ConsoleSessionID;call.answer=answer
 if decision=="approve"{call.humanApproved=true}
 signalToolCall(call)
 return makeApprovalView(a,v),nil
}

func invalidateToolTaskTx(tx *sql.Tx,taskID,reason string)error{
 approvals,err:=tx.Exec("UPDATE tool_approvals SET status='invalidated',revision=revision+1 WHERE status IN ('pending','approved','answered') AND invocation_id IN (SELECT id FROM tool_invocations WHERE task_id=?)",taskID)
 if err!=nil{return err}
 invocations,err:=tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE task_id=? AND state IN ('pending','approved','waiting_question')",taskID)
 if err!=nil{return err}
 a,_:=approvals.RowsAffected();i,_:=invocations.RowsAffected()
 if a+i==0{return nil}
 return insertAutomationEvent(tx,"tools.invalidated",taskID,"",map[string]string{"reason":reason})
}
func (r *Relay) invalidateToolTaskLocked(taskID,reason string){
 tx,err:=r.db.Begin();if err==nil{if err=invalidateToolTaskTx(tx,taskID,reason);err==nil{err=tx.Commit()}else{tx.Rollback()}}
 if b:=r.toolBrokers[taskID];b!=nil{for _,call:=range b.calls{if !call.done{signalToolCall(call)}}}
}
func (r *Relay) revokeToolSessionLocked(owner string){
 defer func(){for _,b:=range r.toolBrokers{for _,call:=range b.calls{if !call.done&&call.owner==owner{call.cancel();signalToolCall(call)}}}}()
 tx,err:=r.db.Begin();if err!=nil{return};defer tx.Rollback()
 if _,err=tx.Exec("UPDATE tool_approvals SET status='invalidated',revision=revision+1 WHERE status IN ('pending','approved','answered') AND json_extract(body,'$.owner')=?",owner);err!=nil{return}
 if _,err=tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE state IN ('pending','approved','waiting_question') AND id IN (SELECT invocation_id FROM tool_approvals WHERE status='invalidated' AND json_extract(body,'$.owner')=?)",owner);err!=nil{return}
 if err=tx.Commit();err!=nil{return}
}
