package relay

import "time"

// Connector sessions retain durable history but do not occupy a started-agent
// slot while idle. Queue pause/delay is not permission to discard queued work.
func (r *Relay) connectorReleaseIdle() {
	if !r.lifecycle.TryLock() {
		return
	}
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	ids := []string{}
	if !r.closed {
		for id, slot := range r.slots {
			if slot.taskID != "" || time.Since(slot.lastUsed) < 60*time.Second {
				continue
			}
			var connectorBinding bool
			if err := r.db.QueryRow("SELECT EXISTS(SELECT 1 FROM profile_bindings b JOIN connector_threads t ON t.conversation_id=b.conversation_id WHERE b.agent_id=?)", id).Scan(&connectorBinding); err != nil || !connectorBinding {
				continue
			}
			agent, err := r.agent(id)
			if err != nil || agent.State != "idle" {
				continue
			}
			var pending int
			if err = r.db.QueryRow("SELECT count(*) FROM tasks WHERE recipient=? AND state IN ('queued','dispatching','running')", id).Scan(&pending); err != nil || pending != 0 {
				continue
			}
			agent.State = "stopping"
			if r.saveAgent(agent) == nil {
				ids = append(ids, id)
			}
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		if err := r.stop(id); err != nil {
			r.logger.Warn("idle connector binding cleanup failed")
		}
	}
}
