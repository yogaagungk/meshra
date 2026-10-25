package relay

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"meshra/internal/model"
)

// These fixtures drive the production broker through a production profile turn:
// the fake harness receives the real Turn.Tools executor and the real broker
// answers. Nothing here asserts on source text, wiring, or echoed model text.

// brokerStep is one native tool call the fake harness issues during a turn.
type brokerStep struct {
	call model.ToolCall
	// gate, when set, blocks the step until the test releases it. It makes
	// "while the first mutation is still outstanding" deterministic.
	gate chan struct{}
	// seq is the position of the step inside one turn's plan.
	seq int
}

// brokerOutcome is the observable result of one broker step.
type brokerOutcome struct {
	taskID string
	callID string
	seq    int
	result model.ToolResult
	err    error
}

// brokerScript is the harness side of a profile turn.
type brokerScript struct {
	plan     func(turn model.Turn) []brokerStep
	parallel bool

	mu      sync.Mutex
	results map[string]brokerOutcome
	notify  chan struct{}
}

func newBrokerScript(steps ...brokerStep) *brokerScript {
	return newBrokerPlan(func(model.Turn) []brokerStep { return steps })
}

func newBrokerPlan(plan func(turn model.Turn) []brokerStep) *brokerScript {
	return &brokerScript{plan: plan, results: map[string]brokerOutcome{}, notify: make(chan struct{}, 64)}
}

func brokerKey(taskID string, seq int) string { return taskID + "|" + strconv.Itoa(seq) }

func (s *brokerScript) record(turn model.Turn, step brokerStep, result model.ToolResult, err error) {
	s.mu.Lock()
	s.results[brokerKey(turn.Task.ID, step.seq)] = brokerOutcome{taskID: turn.Task.ID, callID: step.call.ID, seq: step.seq, result: result, err: err}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *brokerScript) run(ctx context.Context, turn model.Turn, e model.Emit) (string, error) {
	if turn.Tools == nil {
		return "", errors.New("profile turn exposes no tool executor")
	}
	steps := s.plan(turn)
	for i := range steps {
		steps[i].seq = i
	}
	invoke := func(step brokerStep) {
		if step.gate != nil {
			select {
			case <-step.gate:
			case <-ctx.Done():
				s.record(turn, step, model.ToolResult{}, ctx.Err())
				return
			}
		}
		result, err := turn.Tools.Call(ctx, step.call)
		s.record(turn, step, result, err)
	}
	if s.parallel {
		var wg sync.WaitGroup
		for _, step := range steps {
			wg.Add(1)
			go func(step brokerStep) {
				defer wg.Done()
				invoke(step)
			}(step)
		}
		wg.Wait()
		return "broker turn finished", nil
	}
	for _, step := range steps {
		invoke(step)
	}
	return "broker turn finished", nil
}

// await waits for one step result of one turn.
func (s *brokerScript) await(t *testing.T, taskID string, seq int, timeout time.Duration) brokerOutcome {
	t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		outcome, ok := s.results[brokerKey(taskID, seq)]
		s.mu.Unlock()
		if ok {
			return outcome
		}
		select {
		case <-s.notify:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("broker step %d of task %s never finished", seq, taskID)
		}
	}
}

// pending reports whether a step result is already known.
func (s *brokerScript) pending(taskID string, seq int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.results[brokerKey(taskID, seq)]
	return ok
}

// brokerProfile creates a profile with exactly the requested capabilities.
func brokerProfile(t *testing.T, r *Relay, caps model.ProfileCapabilities) model.Profile {
	t.Helper()
	workspace := profileStoreWorkspace(t, r)
	request := profileStoreRequest(workspace)
	request["capabilities"] = caps
	return profileStoreCreate(t, r, request)
}

// brokerConsole installs a console session validator and returns its principal.
func brokerConsole(r *Relay, session string) model.Principal {
	r.SetConsoleSessionValidator(func(id string) bool { return id == session })
	return model.Principal{Admin: true, ConsoleSessionID: session}
}

// brokerConversation creates a profile conversation of the given kind.
func brokerConversation(t *testing.T, r *Relay, kind string, profiles ...model.Profile) model.Conversation {
	t.Helper()
	ids := make([]string, 0, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}
	body, err := json.Marshal(map[string]any{"title": "Broker conversation", "kind": kind, "profileIds": ids})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.profileContinuityOperation(context.Background(), admin, "profiles.createConversation", body)
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Conversation)
}

// brokerSend admits one profile turn owned by the given principal.
func brokerSend(t *testing.T, r *Relay, p model.Principal, profile model.Profile, conversation model.Conversation, text string) model.Task {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": profile.ID, "conversationId": conversation.ID, "text": text, "idempotencyKey": randomID()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.profileSend(context.Background(), p, body)
	if err != nil {
		t.Fatal(err)
	}
	return v.(model.Task)
}

// awaitBrokerPendingApproval waits for the pending attention record of a task.
func awaitBrokerPendingApproval(t *testing.T, r *Relay, taskID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var id string
		err := r.db.QueryRow(`SELECT a.id FROM tool_approvals a JOIN tool_invocations i ON i.id=a.invocation_id WHERE i.task_id=? AND a.status='pending'`, taskID).Scan(&id)
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never produced a pending attention record (%v)", taskID, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitBrokerApprovalStatus waits until an attention record reaches a status.
func awaitBrokerApprovalStatus(t *testing.T, r *Relay, id string, statuses ...string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var status string
		if err := r.db.QueryRow("SELECT status FROM tool_approvals WHERE id=?", id).Scan(&status); err == nil {
			for _, want := range statuses {
				if status == want {
					return status
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("attention record %s never reached %v", id, statuses)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// brokerDecision calls an approval/question automation operation.
func brokerDecision(t *testing.T, r *Relay, p model.Principal, operation string, payload map[string]any) (any, error) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.Automation(context.Background(), p, operation, body)
}

// brokerAttentionBody reads the persisted body of an attention record.
func brokerAttentionBody(t *testing.T, r *Relay, approvalID string) toolApproval {
	t.Helper()
	var body string
	if err := r.db.QueryRow("SELECT body FROM tool_approvals WHERE id=?", approvalID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record toolApproval
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// brokerActionHash reads the recorded action hash of an attention record.
func brokerActionHash(t *testing.T, r *Relay, approvalID string) string {
	t.Helper()
	var hash string
	err := r.db.QueryRow(`SELECT i.action_hash FROM tool_approvals a JOIN tool_invocations i ON i.id=a.invocation_id WHERE a.id=?`, approvalID).Scan(&hash)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// brokerRetypeAttentionAsApproval retypes a live attention record as an
// approval-kind record. The sandbox runtime that produces write/command
// approvals needs bubblewrap and delegated cgroups, which this environment does
// not provide; the record itself, its action hash, revision guard and the live
// broker call it resolves against all stay production state.
func brokerRetypeAttentionAsApproval(t *testing.T, r *Relay, approvalID string) {
	t.Helper()
	var body string
	if err := r.db.QueryRow("SELECT body FROM tool_approvals WHERE id=?", approvalID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record toolApproval
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	record.Kind = "approval"
	updated, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec("UPDATE tool_approvals SET body=? WHERE id=?", string(updated), approvalID); err != nil {
		t.Fatal(err)
	}
}

func brokerStatus(err error) int {
	var e *model.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func brokerText(result model.ToolResult) string {
	out := ""
	for _, content := range result.Content {
		out += content.Text
	}
	return out
}

func TestBrokerRefusesToolsOutsideTheAdmittedCatalog(t *testing.T) {
	script := newBrokerScript(
		brokerStep{call: model.ToolCall{ID: "call-unknown", Name: "meshra_not_a_tool", Arguments: json.RawMessage(`{}`)}},
		brokerStep{call: model.ToolCall{ID: "call-disabled", Name: "meshra_workspace_command", Arguments: json.RawMessage(`{"argv":["/bin/true"]}`)}},
		brokerStep{call: model.ToolCall{ID: "call-extra", Name: "meshra_history_search", Arguments: json.RawMessage(`{"query":"alpha","unexpected":"field"}`)}},
	)
	r, _ := continuityOpenTest(t, script.run)
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, admin, profile, conversation, "probe the catalog")
	done := awaitTask(t, r, task.ID)
	if done.State != "completed" {
		t.Fatalf("profile turn did not complete: %+v", done)
	}
	unknown := script.await(t, task.ID, 0, 10*time.Second)
	disabled := script.await(t, task.ID, 1, 10*time.Second)
	extra := script.await(t, task.ID, 2, 10*time.Second)
	if brokerStatus(unknown.err) != 403 {
		t.Fatalf("unknown tool was not refused with 403: %v", unknown.err)
	}
	if brokerStatus(disabled.err) != 403 {
		t.Fatalf("tool outside the admitted catalog was not refused with 403: %v", disabled.err)
	}
	if brokerStatus(extra.err) != 400 {
		t.Fatalf("unknown argument field was not refused with 400: %v", extra.err)
	}
	var invocations int
	if err := r.db.QueryRow("SELECT count(*) FROM tool_invocations WHERE task_id=?", task.ID).Scan(&invocations); err != nil {
		t.Fatal(err)
	}
	if invocations != 0 {
		t.Fatalf("refused calls reached an action: %d invocations", invocations)
	}
	var approvals int
	if err := r.db.QueryRow("SELECT count(*) FROM tool_approvals").Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("refused calls created attention records: %d", approvals)
	}
}

func TestBrokerAttentionRequiresLiveConsoleAuthority(t *testing.T) {
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask the operator")
	approval := awaitBrokerPendingApproval(t, r, task.ID)

	// A bearer principal carries no console provenance.
	for _, principal := range []model.Principal{{Admin: true}, {AgentID: task.To}} {
		_, err := brokerDecision(t, r, principal, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "green"})
		requireStatus(t, err, 403)
		_, err = brokerDecision(t, r, principal, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": "wrong", "idempotencyKey": "decision-key"})
		requireStatus(t, err, 403)
		_, err = brokerDecision(t, r, principal, "approvals.list", map[string]any{})
		requireStatus(t, err, 403)
	}

	// A stale expected revision is refused even for a live console session.
	_, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 99, "answer": "green"})
	requireStatus(t, err, 409)

	// A decision that does not match the reviewed action hash is refused.
	brokerRetypeAttentionAsApproval(t, r, approval)
	hash := brokerActionHash(t, r, approval)
	_, err = brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": "not-the-reviewed-action", "idempotencyKey": "decision-key"})
	requireStatus(t, err, 409)
	var status, decision string
	if err := r.db.QueryRow("SELECT status,body FROM tool_approvals WHERE id=?", approval).Scan(&status, &decision); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("a refused action hash changed the attention record: %s", status)
	}
	var recorded toolApproval
	if err := json.Unmarshal([]byte(decision), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Decision != "" {
		t.Fatalf("a refused action hash recorded a decision: %+v", recorded)
	}

	// The reviewed action hash is accepted and the pending call resumes.
	view, err := brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": hash, "idempotencyKey": "decision-key"})
	if err != nil {
		t.Fatalf("reviewed action hash was refused: %v", err)
	}
	approved, ok := view.(approvalView)
	if !ok || approved.Status != "approved" {
		t.Fatalf("decision did not record an approval: %+v", view)
	}
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	if outcome.err != nil {
		t.Fatalf("approved call did not resume: %v", outcome.err)
	}
	awaitBrokerApprovalStatus(t, r, approval, "completed")
}

func TestBrokerRepeatedAndConflictingDecisions(t *testing.T) {
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask the operator")
	approval := awaitBrokerPendingApproval(t, r, task.ID)

	first, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "chartreuse-9f2b"})
	if err != nil {
		t.Fatalf("first answer was refused: %v", err)
	}
	repeated, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "chartreuse-9f2b"})
	if err != nil {
		t.Fatalf("identical repeated answer was refused: %v", err)
	}
	firstJSON, _ := json.Marshal(first)
	repeatedJSON, _ := json.Marshal(repeated)
	if string(firstJSON) != string(repeatedJSON) {
		t.Fatalf("identical repeated answer did not return the recorded decision: %s vs %s", firstJSON, repeatedJSON)
	}
	var revision int
	var status string
	if err := r.db.QueryRow("SELECT revision,status FROM tool_approvals WHERE id=?", approval).Scan(&revision, &status); err != nil {
		t.Fatal(err)
	}
	if status != "answered" || revision != 2 {
		t.Fatalf("repeated answer was applied twice: status=%s revision=%d", status, revision)
	}
	recorded := brokerAttentionBody(t, r, approval)

	// A different answer for the same record is refused.
	_, err = brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "chartreuse-different"})
	requireStatus(t, err, 409)
	// A different decision kind for the same record is refused too.
	_, err = brokerDecision(t, r, console, "approvals.decide", map[string]any{"id": approval, "expectedRevision": 1, "decision": "approve", "actionHash": brokerActionHash(t, r, approval), "idempotencyKey": "decision-key"})
	requireStatus(t, err, 409)
	after := brokerAttentionBody(t, r, approval)
	if after.Decision != recorded.Decision || after.AnswerHash != recorded.AnswerHash || after.DecisionRevision != recorded.DecisionRevision {
		t.Fatalf("a conflicting decision changed the recorded answer: %+v vs %+v", after, recorded)
	}
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	if outcome.err != nil {
		t.Fatalf("answered call did not resume: %v", outcome.err)
	}
	if !strings.Contains(brokerText(outcome.result), "chartreuse-9f2b") {
		t.Fatalf("the answering turn did not receive the answer: %q", brokerText(outcome.result))
	}
}

func TestBrokerQuestionAnswerIsNotPersistedAndResumesOnlyItsOwnCall(t *testing.T) {
	script := newBrokerPlan(func(turn model.Turn) []brokerStep {
		return []brokerStep{{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}}}
	})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	// Two profiles own two workspaces, so the two turns hold two distinct
	// repository locks and can be pending at the same time.
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	other := brokerProfile(t, r, model.ProfileCapabilities{})
	first := brokerConversation(t, r, "chat", profile)
	second := brokerConversation(t, r, "chat", other)
	firstTask := brokerSend(t, r, console, profile, first, "first")
	secondTask := brokerSend(t, r, console, other, second, "second")
	firstApproval := awaitBrokerPendingApproval(t, r, firstTask.ID)
	secondApproval := awaitBrokerPendingApproval(t, r, secondTask.ID)
	if firstApproval == secondApproval {
		t.Fatal("two turns shared one attention record")
	}

	const answer = "chartreuse-9f2b"
	if _, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": firstApproval, "expectedRevision": 1, "answer": answer}); err != nil {
		t.Fatalf("answer was refused: %v", err)
	}
	firstOutcome := script.await(t, firstTask.ID, 0, 20*time.Second)
	if firstOutcome.err != nil {
		t.Fatalf("the owning call did not resume: %v", firstOutcome.err)
	}
	if !strings.Contains(brokerText(firstOutcome.result), answer) {
		t.Fatalf("the owning call did not receive the answer: %q", brokerText(firstOutcome.result))
	}
	if script.pending(secondTask.ID, 0) {
		t.Fatal("answering one attention record resumed another turn's call")
	}
	var secondStatus string
	if err := r.db.QueryRow("SELECT status FROM tool_approvals WHERE id=?", secondApproval).Scan(&secondStatus); err != nil {
		t.Fatal(err)
	}
	if secondStatus != "pending" {
		t.Fatalf("answering one attention record changed another record: %s", secondStatus)
	}

	// The raw answer is never persisted.
	var invocationBodies, approvalBodies, eventData []string
	rows, err := r.db.Query("SELECT body FROM tool_invocations")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		invocationBodies = append(invocationBodies, body)
	}
	rows.Close()
	rows, err = r.db.Query("SELECT body FROM tool_approvals")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		approvalBodies = append(approvalBodies, body)
	}
	rows.Close()
	rows, err = r.db.Query("SELECT data FROM automation_events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		eventData = append(eventData, data)
	}
	rows.Close()
	for _, body := range invocationBodies {
		if strings.Contains(body, answer) {
			t.Fatalf("the raw answer was persisted in a tool invocation: %s", body)
		}
	}
	for _, body := range approvalBodies {
		if strings.Contains(body, answer) {
			t.Fatalf("the raw answer was persisted in an attention record: %s", body)
		}
	}
	for _, data := range eventData {
		if strings.Contains(data, answer) {
			t.Fatalf("the raw answer was persisted in an automation event: %s", data)
		}
	}
	// The record keeps a hash of the answer instead.
	var answerHash string
	if err := r.db.QueryRow("SELECT body FROM tool_approvals WHERE id=?", firstApproval).Scan(&answerHash); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answerHash, tokenHash(answer)) {
		t.Fatalf("the attention record does not keep the answer hash: %s", answerHash)
	}
}

func TestBrokerNativeCallIdentifierReuseIsRefused(t *testing.T) {
	script := newBrokerScript(
		brokerStep{call: model.ToolCall{ID: "call-save", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"alpha note"}`)}},
		brokerStep{call: model.ToolCall{ID: "call-save", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"beta note"}`)}},
		brokerStep{call: model.ToolCall{ID: "call-save", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"alpha note"}`)}},
	)
	r, _ := continuityOpenTest(t, script.run)
	profile := brokerProfile(t, r, model.ProfileCapabilities{MemoryWrite: true})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, admin, profile, conversation, "save a note")
	done := awaitTask(t, r, task.ID)
	if done.State != "completed" {
		t.Fatalf("profile turn did not complete: %+v", done)
	}
	first := script.await(t, task.ID, 0, 10*time.Second)
	reused := script.await(t, task.ID, 1, 10*time.Second)
	replayed := script.await(t, task.ID, 2, 10*time.Second)
	if first.err != nil {
		t.Fatalf("the first call failed: %v", first.err)
	}
	// The same native call identifier with different arguments is refused.
	if brokerStatus(reused.err) != 409 {
		t.Fatalf("a reused call identifier with different arguments was not refused with 409: %v", reused.err)
	}
	// An identical completed call returns the recorded result.
	if replayed.err != nil {
		t.Fatalf("an identical replayed call was refused: %v", replayed.err)
	}
	firstJSON, _ := json.Marshal(first.result)
	replayedJSON, _ := json.Marshal(replayed.result)
	if string(firstJSON) != string(replayedJSON) {
		t.Fatalf("the replayed call did not return the recorded result: %s vs %s", firstJSON, replayedJSON)
	}
	var invocations int
	if err := r.db.QueryRow("SELECT count(*) FROM tool_invocations WHERE task_id=?", task.ID).Scan(&invocations); err != nil {
		t.Fatal(err)
	}
	if invocations != 1 {
		t.Fatalf("a reused call identifier produced extra invocations: %d", invocations)
	}
	var memories int
	if err := r.db.QueryRow("SELECT count(*) FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 1 {
		t.Fatalf("a replayed call repeated its action: %d memories", memories)
	}
	var content string
	if err := r.db.QueryRow("SELECT content FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "alpha note" {
		t.Fatalf("the replayed call changed the stored content: %q", content)
	}
}

func TestBrokerTaskCancellationTerminatesPendingAttention(t *testing.T) {
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask the operator")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	if _, err := r.Cancel(admin, task.ID); err != nil {
		t.Fatal(err)
	}
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	if outcome.err == nil {
		t.Fatalf("the pending call completed after the task was cancelled: %+v", outcome.result)
	}
	if !errors.Is(outcome.err, context.Canceled) && brokerStatus(outcome.err) != 409 {
		t.Fatalf("the pending call did not end with a cancellation or an invalidated decision: %v", outcome.err)
	}
	status := awaitBrokerApprovalStatus(t, r, approval, "invalidated", "cancelled", "expired", "denied", "completed")
	if status == "pending" {
		t.Fatalf("the attention record is still pending after cancellation: %s", status)
	}
}

func TestBrokerConsoleRevocationTerminatesPendingAttention(t *testing.T) {
	script := newBrokerScript(brokerStep{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}})
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	profile := brokerProfile(t, r, model.ProfileCapabilities{})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask the operator")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	r.RevokeConsoleSession(console.ConsoleSessionID)
	outcome := script.await(t, task.ID, 0, 20*time.Second)
	if outcome.err == nil {
		t.Fatalf("the pending call completed after the console session was revoked: %+v", outcome.result)
	}
	if !errors.Is(outcome.err, context.Canceled) && brokerStatus(outcome.err) != 409 {
		t.Fatalf("the pending call did not end with a cancellation or an invalidated decision: %v", outcome.err)
	}
	status := awaitBrokerApprovalStatus(t, r, approval, "invalidated", "cancelled", "expired", "denied", "completed")
	if status == "pending" {
		t.Fatalf("the attention record is still pending after revocation: %s", status)
	}
	_, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "green"})
	requireStatus(t, err, 403)
}

func TestBrokerAllowsOnlyOneOutstandingMutation(t *testing.T) {
	gate := make(chan struct{})
	later := make(chan struct{})
	script := newBrokerScript(
		brokerStep{call: model.ToolCall{ID: "call-question", Name: "meshra_ask_user", Arguments: json.RawMessage(`{"question":"Which color?"}`)}},
		brokerStep{call: model.ToolCall{ID: "call-write", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"second mutation"}`)}, gate: gate},
		brokerStep{call: model.ToolCall{ID: "call-write-later", Name: "meshra_memory_save", Arguments: json.RawMessage(`{"content":"later mutation"}`)}, gate: later},
	)
	script.parallel = true
	r, _ := continuityOpenTest(t, script.run)
	console := brokerConsole(r, "broker-session")
	profile := brokerProfile(t, r, model.ProfileCapabilities{MemoryWrite: true})
	conversation := brokerConversation(t, r, "chat", profile)
	task := brokerSend(t, r, console, profile, conversation, "ask and write")
	approval := awaitBrokerPendingApproval(t, r, task.ID)
	close(gate)
	write := script.await(t, task.ID, 1, 20*time.Second)
	if brokerStatus(write.err) != 409 {
		t.Fatalf("a second outstanding mutation was not refused with 409: %v", write.err)
	}
	var memories int
	if err := r.db.QueryRow("SELECT count(*) FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 0 {
		t.Fatalf("the refused mutation reached its action: %d memories", memories)
	}
	if _, err := brokerDecision(t, r, console, "questions.answer", map[string]any{"id": approval, "expectedRevision": 1, "answer": "green"}); err != nil {
		t.Fatalf("the first attention record was refused: %v", err)
	}
	question := script.await(t, task.ID, 0, 20*time.Second)
	if question.err != nil {
		t.Fatalf("the first mutation did not resume: %v", question.err)
	}
	// Once the outstanding mutation settles the next one is admitted.
	close(later)
	next := script.await(t, task.ID, 2, 20*time.Second)
	if next.err != nil {
		t.Fatalf("a mutation after the first one settled was refused: %v", next.err)
	}
	if err := r.db.QueryRow("SELECT count(*) FROM profile_memories WHERE profile_id=?", profile.ID).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 1 {
		t.Fatalf("the settled mutation did not release the outstanding slot: %d memories", memories)
	}
}
