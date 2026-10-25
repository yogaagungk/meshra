package relay

import "time"

// recoverTools runs before scheduler startup. A claimed call may have produced
// effects; neither recovery nor a reconnected model is allowed to repeat it.
func (r *Relay) recoverTools()error{
 tx,err:=r.db.Begin();if err!=nil{return err};defer tx.Rollback()
 if _,err=tx.Exec("UPDATE tool_approvals SET status='invalidated',revision=revision+1 WHERE status IN ('pending','approved','answered')");err!=nil{return err}
 if _,err=tx.Exec("UPDATE tool_approvals SET status='uncertain',revision=revision+1 WHERE status='claimed'");err!=nil{return err}
 if _,err=tx.Exec("UPDATE tool_invocations SET state='uncertain' WHERE state='claimed'");err!=nil{return err}
 if _,err=tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE state IN ('pending','approved','waiting_question')");err!=nil{return err}
 return tx.Commit()
}

// invalidateProfileToolPolicy wakes every outstanding call of a profile whose
// admitted catalog was narrowed. The persisted attention rows are invalidated by
// the policy trigger, so the woken call reports a stale decision instead of
// waiting for its expiry. The caller holds r.mu.
func (r *Relay) invalidateProfileToolPolicy(profileID string){
 for _,b:=range r.toolBrokers{
  if b.snapshot.Profile.ID!=profileID{continue}
  for _,call:=range b.calls{
   if !call.done{signalToolCall(call)}
  }
 }
}

func (r *Relay) pruneToolHistory(before time.Time)error{
 cutoff:=before.UTC().Format(time.RFC3339Nano)
 tx,err:=r.db.Begin();if err!=nil{return err};defer tx.Rollback()
 if _,err=tx.Exec(`DELETE FROM tool_invocations WHERE state IN ('completed','failed','cancelled','denied') AND json_extract(body,'$.createdAt')<? AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=tool_invocations.task_id AND t.state IN ('completed','failed','cancelled','timed_out')) AND NOT EXISTS(SELECT 1 FROM workspace_changesets c WHERE c.invocation_id=tool_invocations.id)`,cutoff);err!=nil{return err}
 if _,err=tx.Exec("DELETE FROM attachment_read_audit WHERE time<?",cutoff);err!=nil{return err}
 return tx.Commit()
}
