package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"meshra/internal/model"
)

// Caller holds relay ownership; no native runtime is started by materialization.
func (r *Relay) profileBinding(profileID, conversationID string) (model.ProfileBinding, error) {
	var b model.ProfileBinding
	var body string
	err := r.db.QueryRow("SELECT body FROM profile_bindings WHERE profile_id=? AND conversation_id=? ORDER BY generation DESC LIMIT 1", profileID, conversationID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return b, model.Err(404, "profile binding not found")
	}
	if err != nil {
		return b, err
	}
	err = json.Unmarshal([]byte(body), &b)
	return b, err
}

func (r *Relay) currentProfileAgent(agentID string) error {
	var profileID, conversationID string
	if err := r.db.QueryRow("SELECT profile_id,conversation_id FROM profile_bindings WHERE agent_id=?", agentID).Scan(&profileID, &conversationID); err != nil {
		return model.Err(403, "identity is not a current profile binding")
	}
	b, err := r.profileBinding(profileID, conversationID)
	if err != nil {
		return err
	}
	if b.AgentID != agentID {
		return model.Err(403, "historical profile binding cannot be controlled or receive new work")
	}
	return nil
}

func (r *Relay) prepareProfileBinding(p model.Profile, conversationID string, generation int) (model.ProfileBinding, model.Agent, error) {
	var b model.ProfileBinding
	if len(p.ID) != 32 || len(conversationID) != 32 || generation < 1 || generation > 1000000000 {
		return b, model.Agent{}, model.Err(400, "invalid profile binding identity")
	}
	w, err := r.workspace(p.WorkspaceID)
	if err != nil {
		return b, model.Agent{}, err
	}
	name := fmt.Sprintf("profile-%s-%s-%d", p.ID[:8], conversationID[:8], generation)
	a, err := r.prepareAgentLocked(model.Principal{Admin: true}, model.CreateAgent{Harness: p.Binding.Harness, Name: name, DisplayName: p.Name, Cwd: w.Path, Config: p.Binding.Config, Policy: model.Policy{Destinations: []string{}, Operations: []string{"list", "send", "request", "status", "events", "cancel"}, Delegate: true, Senders: []string{}, InboundOperations: []string{"send", "request"}}})
	if err != nil {
		return b, a, err
	}
	a.ProfileManaged = true
	b = model.ProfileBinding{ID: randomID(), ProfileID: p.ID, ConversationID: conversationID, Generation: generation, AgentID: a.ID, Harness: p.Binding.Harness, Config: p.Binding.Config, ConnectionID: p.Binding.ConnectionID, ToolCatalogDigest: toolCatalogDigest(profileToolCatalog(p))}
	b.ExternalConversation, err = r.conversationExternalLocked(conversationID)
	if err != nil {
		return b, a, err
	}
	return b, a, nil
}

func (r *Relay) insertProfileBinding(tx *sql.Tx, b model.ProfileBinding, a model.Agent) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO agents(id,body) VALUES(?,?)", a.ID, string(body)); err != nil {
		return err
	}
	body, err = json.Marshal(b)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO profile_bindings(id,profile_id,conversation_id,generation,agent_id,body) VALUES(?,?,?,?,?,?)", b.ID, b.ProfileID, b.ConversationID, b.Generation, b.AgentID, string(body)); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO profile_context_epochs(binding_id,epoch,reset_after_seq) VALUES(?,1,0)", b.ID); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR IGNORE INTO conversation_participants(conversation_id,agent_id) VALUES(?,?)", b.ConversationID, b.AgentID)
	return err
}

// Only generated policies are reconciled; named-agent policies are never edited.
func (r *Relay) syncProfilePolicies(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT b.body,p.body,a.body FROM profile_bindings b JOIN agent_profiles p ON p.id=b.profile_id JOIN agents a ON a.id=b.agent_id WHERE b.generation=(SELECT max(n.generation) FROM profile_bindings n WHERE n.profile_id=b.profile_id AND n.conversation_id=b.conversation_id)`)
	if err != nil {
		return err
	}
	type record struct {
		binding model.ProfileBinding
		profile model.Profile
		agent   model.Agent
	}
	records := []record{}
	for rows.Next() {
		var b, p, a string
		var v record
		if err = rows.Scan(&b, &p, &a); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(b), &v.binding); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(p), &v.profile); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(a), &v.agent); err != nil {
			break
		}
		records = append(records, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	byConversation := make(map[string]map[string]*record)
	for i := range records {
		v := &records[i]
		v.agent.Policy.Destinations = []string{}
		v.agent.Policy.Senders = []string{}
		peers := byConversation[v.binding.ConversationID]
		if peers == nil {
			peers = make(map[string]*record)
			byConversation[v.binding.ConversationID] = peers
		}
		peers[v.profile.ID] = v
	}
	for i := range records {
		v := &records[i]
		if v.profile.Archived {
			continue
		}
		for _, id := range v.profile.AllowedProfileIDs {
			target := byConversation[v.binding.ConversationID][id]
			if target == nil || target.profile.Archived {
				continue
			}
			v.agent.Policy.Destinations = append(v.agent.Policy.Destinations, target.agent.ID)
			target.agent.Policy.Senders = append(target.agent.Policy.Senders, v.agent.ID)
		}
	}
	for i := range records {
		a := &records[i].agent
		sort.Strings(a.Policy.Destinations)
		sort.Strings(a.Policy.Senders)
		body, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE agents SET body=? WHERE id=?", string(body), a.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *Relay) profileSnapshot(t model.Task) (model.ProfileTaskSnapshot, error) {
	var snapshot model.ProfileTaskSnapshot
	var body string
	if err := r.db.QueryRow("SELECT body FROM profile_task_snapshots WHERE task_id=?", t.ID).Scan(&body); err != nil {
		return snapshot, err
	}
	err := json.Unmarshal([]byte(body), &snapshot)
	if err == nil && !snapshot.External {
		var external bool
		readErr := r.db.QueryRow("SELECT coalesce(json_extract(body,'$.externalConversation'),1) FROM profile_bindings WHERE id=?", snapshot.Binding.ID).Scan(&external)
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return snapshot, readErr
		}
		snapshot.External = external
	}
	return snapshot, err
}

func insertAutomationEvent(tx *sql.Tx, kind, resourceID, profileID string, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(body) > 8192 {
		return model.Err(413, "automation event metadata too large")
	}
	_, err = tx.Exec("INSERT INTO automation_events(kind,resource_id,profile_id,data,time) VALUES(?,?,?,?,?)", kind, resourceID, profileID, redact(string(body)), now().Format(time.RFC3339Nano))
	return err
}
