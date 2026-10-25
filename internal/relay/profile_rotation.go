package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"meshra/internal/model"
)

type profileEpoch struct {
	Epoch         int   `json:"epoch"`
	ResetAfterSeq int64 `json:"resetAfterSeq"`
}

func (r *Relay) pendingProfileRotations() (map[string]bool, error) {
	rows, err := r.db.Query(`SELECT b.agent_id FROM profile_context_epochs e JOIN profile_bindings b ON b.id=e.binding_id WHERE e.rotate_pending=1 AND b.generation=(SELECT max(n.generation) FROM profile_bindings n WHERE n.profile_id=b.profile_id AND n.conversation_id=b.conversation_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (r *Relay) profileRotationNeeded(agentID string) (bool, error) {
	var pending bool
	err := r.db.QueryRow("SELECT e.rotate_pending FROM profile_context_epochs e JOIN profile_bindings b ON b.id=e.binding_id WHERE b.agent_id=?", agentID).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return pending, err
}

// Caller holds lifecycle and r.mu, after complete native teardown. Task/session
// ownership is immutable; only the backing agent's next native session changes.
func (r *Relay) advanceProfileEpochLocked(agentID string, cutoff *int64) (profileEpoch, error) {
	var result profileEpoch
	var bindingID, profileID string
	if err := r.db.QueryRow("SELECT id,profile_id FROM profile_bindings WHERE agent_id=?", agentID).Scan(&bindingID, &profileID); err != nil {
		return result, err
	}
	a, err := r.agent(agentID)
	if err != nil {
		return result, err
	}
	a.SessionID = ""
	a.LastSeen = now()
	body, err := json.Marshal(a)
	if err != nil {
		return result, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if cutoff == nil {
		_, err = tx.Exec("UPDATE profile_context_epochs SET epoch=epoch+1,rotate_pending=0 WHERE binding_id=?", bindingID)
	} else {
		_, err = tx.Exec("UPDATE profile_context_epochs SET epoch=epoch+1,rotate_pending=0,reset_after_seq=? WHERE binding_id=?", *cutoff, bindingID)
	}
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec("UPDATE agents SET body=? WHERE id=?", string(body), agentID); err != nil {
		return result, err
	}
	if _, err = tx.Exec("DELETE FROM credentials WHERE agent_id=?", agentID); err != nil {
		return result, err
	}
	if err = tx.QueryRow("SELECT epoch,reset_after_seq FROM profile_context_epochs WHERE binding_id=?", bindingID).Scan(&result.Epoch, &result.ResetAfterSeq); err != nil {
		return result, err
	}
	if err = insertAutomationEvent(tx, "profiles.contextReset", bindingID, profileID, result); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// Called under r.mu by the scheduler. The goroutine never waits on lifecycle:
// Close owns lifecycle while waiting for workers and must not deadlock with it.
func (r *Relay) beginProfileRotation(agentID string) {
	if r.rotatingProfiles == nil {
		r.rotatingProfiles = make(map[string]bool)
	}
	if r.rotatingProfiles[agentID] {
		return
	}
	r.rotatingProfiles[agentID] = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if !r.lifecycle.TryLock() {
			r.mu.Lock()
			delete(r.rotatingProfiles, agentID)
			closed := r.closed
			r.mu.Unlock()
			if !closed {
				time.AfterFunc(time.Second, r.signal)
			}
			return
		}
		defer r.lifecycle.Unlock()
		repositoryLock := ""
		defer func() {
			r.mu.Lock()
			delete(r.rotatingProfiles, agentID)
			if repositoryLock != "" {
				delete(r.repos, repositoryLock)
			}
			r.mu.Unlock()
			r.signal()
		}()
		r.mu.Lock()
		s := r.slots[agentID]
		if r.closed || s == nil || s.taskID != "" {
			r.mu.Unlock()
			return
		}
		pending, err := r.profileRotationNeeded(agentID)
		if err != nil || !pending || r.repos[s.repo] {
			r.mu.Unlock()
			return
		}
		repositoryLock = s.repo
		r.repos[repositoryLock] = true
		r.mu.Unlock()
		if err = r.stop(agentID); err != nil {
			r.mu.Lock()
			a, e := r.agent(agentID)
			if e == nil {
				a.State = "error"
				a.Error = "context rotation teardown failed; inspect runtime before restarting"
				_ = r.saveAgent(a)
			}
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		_, err = r.advanceProfileEpochLocked(agentID, nil)
		r.mu.Unlock()
		if err != nil {
			return
		}
		_, _ = r.actionWithLifecycle(model.Principal{Admin: true}, agentID, "start", true)
	}()
}

func (r *Relay) profileResetContext(p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q struct {
		ID               string `json:"id"`
		ConversationID   string `json:"conversationId"`
		ExpectedRevision *int   `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ExpectedRevision == nil {
		return nil, model.Err(400, "expectedRevision is required")
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	profile, err := r.profile(q.ID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if profile.Revision != *q.ExpectedRevision {
		r.mu.Unlock()
		return nil, model.Err(409, "profile revision changed")
	}
	if err = r.profileBusy(profile.ID); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	binding, err := r.profileBinding(profile.ID, q.ConversationID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	var cutoff int64
	if err = r.db.QueryRow("SELECT coalesce(max(seq),0) FROM tasks WHERE json_extract(body,'$.conversationId')=?", q.ConversationID).Scan(&cutoff); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	started := r.slots[binding.AgentID] != nil
	r.mu.Unlock()
	if err = r.stop(binding.AgentID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	result, err := r.advanceProfileEpochLocked(binding.AgentID, &cutoff)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if started && !profile.Archived {
		_, _ = r.actionWithLifecycle(p, binding.AgentID, "start", true)
	}
	r.signal()
	return result, nil
}
