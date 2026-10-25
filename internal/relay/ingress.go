package relay

import (
	"meshra/internal/connectors"
	"meshra/internal/model"
	"slices"
)

// ingressGrant is process-internal authority for one durable reviewed route or
// run reservation. HTTP payloads and bearer principals cannot mint it.
type ingressGrant struct {
	source            string
	connectorID       string
	connectorRevision int
	route             connectorRoute
	event             connectors.Event
	conversationID    string
	binding           model.ProfileBinding
	routine           *routineIngressGrant
}

// validate runs under relay ownership immediately before admission.
func (g *ingressGrant) validate(r *Relay, q model.SendRequest) error {
	if g == nil || q.Type != "request" || q.To != g.binding.AgentID || q.ConversationID != g.conversationID || q.From != "" || q.ParentTaskID != "" || len(q.ContextRefs) != 0 {
		return model.Err(403, "ingress grant does not authorize this request")
	}
	if g.routine != nil {
		return g.validateRoutine(r, q)
	}
	if g.source != "connector:"+g.connectorID || len(q.DependsOn) != 0 {
		return model.Err(403, "invalid connector ingress grant")
	}
	record, err := r.connectorRecordLocked(g.connectorID)
	if err != nil {
		return err
	}
	if !record.Enabled {
		return model.Err(403, "connector disabled")
	}
	if g.connectorRevision <= 0 || record.Revision != g.connectorRevision {
		return model.Err(409, "connector configuration changed")
	}
	for _, name := range connectorSecretNames(connectors.Kind(record.Kind)) {
		status, err := r.secretStatus(r.ctx, record.Secrets[name])
		if err != nil {
			return err
		}
		if status != "saved" {
			return model.Err(403, "connector credentials are unavailable")
		}
	}
	routes, err := r.connectorRoutesLocked(g.connectorID)
	if err != nil {
		return err
	}
	matched, ok := matchConnectorRoute(routes, g.event)
	if !ok || matched.ID != g.route.ID || matched.Revision != g.route.Revision || matched.ProfileID != g.binding.ProfileID {
		return model.Err(409, "connector route changed")
	}
	var conversation string
	if err = r.db.QueryRow("SELECT conversation_id FROM connector_threads WHERE connector_id=? AND external_thread_key=?", g.connectorID, g.event.ThreadKey).Scan(&conversation); err != nil {
		return err
	}
	if conversation != g.conversationID {
		return model.Err(403, "connector thread changed")
	}
	profile, err := r.profile(matched.ProfileID)
	if err != nil {
		return err
	}
	if profile.Archived {
		return model.Err(409, "profile archived")
	}
	binding, err := r.profileBinding(profile.ID, conversation)
	if err != nil {
		return err
	}
	if binding.ID != g.binding.ID {
		return model.Err(409, "connector binding changed")
	}
	return nil
}

func (r *Relay) startConnectorBinding(connectorID string, event connectors.Event, route connectorRoute, conversationID string, binding model.ProfileBinding) error {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	record, err := r.connectorRecordLocked(connectorID)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	grant := &ingressGrant{source: "connector:" + connectorID, connectorID: connectorID, connectorRevision: record.Revision, event: event, route: route, conversationID: conversationID, binding: binding}
	err = grant.validate(r, model.SendRequest{Type: "request", To: binding.AgentID, ConversationID: conversationID})
	if err == nil && !route.AutoStart {
		err = model.Err(403, "connector startup not granted")
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = r.actionWithLifecycle(model.Principal{}, binding.AgentID, "start", true)
	return err
}

type routineIngressGrant struct {
	runID     string
	routineID string
	revision  int
	nodeID    string
	text      string
	timeout   int
}

func routineNodeIngress(d routineDefinition, record routineRunRecord, node routineFlowNode, binding model.ProfileBinding, text string) *ingressGrant {
	return &ingressGrant{source: "routine:" + d.ID, conversationID: record.ConversationID, binding: binding, routine: &routineIngressGrant{runID: record.ID, routineID: d.ID, revision: d.Revision, nodeID: node.ID, text: text, timeout: d.TimeoutSeconds}}
}

func routineNodeDependencies(record routineRunRecord, nodeID string) []string {
	if record.Graph == nil {
		return nil
	}
	parents := routineFlowParents(*record.Graph, nodeID)
	tasks := make([]string, 0, len(parents))
	for _, parent := range parents {
		step := routineStepByNode(record.Steps, parent)
		if step != nil && step.TaskID != "" {
			tasks = append(tasks, step.TaskID)
		}
	}
	return tasks
}

func (g *ingressGrant) validateRoutine(r *Relay, q model.SendRequest) error {
	grant := g.routine
	if g.source != "routine:"+grant.routineID || q.Text != grant.text || q.TimeoutSeconds != grant.timeout || len(q.AttachmentIDs) != 0 || q.IdempotencyKey != tokenHash("routine-node\x00"+grant.runID+"\x00"+grant.nodeID) {
		return model.Err(403, "routine ingress does not authorize this action")
	}
	record, err := r.loadRoutineRun(grant.runID)
	if err != nil {
		return err
	}
	if routineTerminal(record.State) || record.BindingsReleased || record.RoutineID != grant.routineID || record.Revision != grant.revision || record.ConversationID != g.conversationID || record.Definition == nil || record.Graph == nil {
		return model.Err(409, "routine run reservation changed")
	}
	if record.Definition.ID != grant.routineID || record.Definition.Revision != grant.revision || record.Definition.TimeoutSeconds != grant.timeout {
		return model.Err(409, "routine admission definition changed")
	}
	step := routineStepByNode(record.Steps, grant.nodeID)
	if step == nil || step.State != routineStepReserved || step.TaskID != "" || step.BindingID != g.binding.ID {
		return model.Err(409, "routine node reservation is no longer active")
	}
	found := false
	for _, node := range record.Graph.Nodes {
		if node.ID == grant.nodeID && node.ProfileID == g.binding.ProfileID {
			found = true
			break
		}
	}
	if !found {
		return model.Err(403, "routine node is outside the admitted graph")
	}
	parents := routineFlowParents(*record.Graph, grant.nodeID)
	for _, parent := range parents {
		previous := routineStepByNode(record.Steps, parent)
		if previous == nil || previous.State != routineStepCompleted || previous.TaskID == "" {
			return model.Err(409, "routine dependency has not succeeded")
		}
		task, err := r.task(previous.TaskID)
		if err != nil {
			return err
		}
		if task.State != "completed" || task.ConversationID != record.ConversationID {
			return model.Err(409, "routine dependency is not successful in this conversation")
		}
	}
	if !slices.Equal(q.DependsOn, routineNodeDependencies(record, grant.nodeID)) {
		return model.Err(403, "routine dependency grant does not match")
	}
	profile, err := r.profile(g.binding.ProfileID)
	if err != nil {
		return err
	}
	if profile.Archived {
		return model.Err(409, "profile archived")
	}
	binding, err := r.profileBinding(profile.ID, g.conversationID)
	if err != nil {
		return err
	}
	if binding.ID != g.binding.ID || binding.AgentID != q.To {
		return model.Err(409, "routine binding changed")
	}
	return nil
}
