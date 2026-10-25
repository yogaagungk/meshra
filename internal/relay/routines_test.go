package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	cronlib "github.com/robfig/cron/v3"
	"meshra/internal/model"
)

// These fixtures exercise the durable routine and flow machinery with the
// deterministic test executor; they make no claim about any installed
// provider, sandbox or scheduler.
func routineOpenTest(t *testing.T, run func(context.Context, model.Turn, model.Emit) (string, error)) (*Relay, string) {
	t.Helper()
	r, dir := openTest(t, run)
	r.mu.Lock()
	for harness, adapter := range r.adapters {
		adapter.Capabilities["profileAutomation"] = true
		r.adapters[harness] = adapter
	}
	r.mu.Unlock()
	return r, dir
}

func routineCall(t *testing.T, r *Relay, operation string, payload any) (any, error) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r.routineOperation(context.Background(), admin, operation, body)
}

func routineProfile(t *testing.T, r *Relay, name string) model.Profile {
	t.Helper()
	request := profileStoreRequest(profileStoreWorkspace(t, r))
	request["name"] = name
	return profileStoreCreate(t, r, request)
}

func routineSave(t *testing.T, r *Relay, routine map[string]any) routineView {
	t.Helper()
	value, err := routineCall(t, r, "routines.save", map[string]any{"routine": routine})
	if err != nil {
		t.Fatal(err)
	}
	return value.(routineView)
}

// routineDue moves a routine's stored next occurrence into the past so the
// durable scan observes it exactly as a live timer would.
func routineDue(t *testing.T, r *Relay, id string, at time.Time) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.db.Exec("UPDATE routines SET next_due_at=? WHERE id=?", routineTime(at), id); err != nil {
		t.Fatal(err)
	}
}

func routineRunsOf(t *testing.T, r *Relay, id string) []routineRunRecord {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query("SELECT body FROM routine_runs WHERE routine_id=? ORDER BY rowid", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	runs := []routineRunRecord{}
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		var record routineRunRecord
		if err = json.Unmarshal([]byte(body), &record); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, record)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return runs
}

func routineReconcile(t *testing.T, r *Relay) {
	t.Helper()
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if err := r.reconcileRoutineRuns(); err != nil {
		t.Fatal(err)
	}
}

func routineAssertStepIndex(t *testing.T, r *Relay, record routineRunRecord) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var runState string
	if err := r.db.QueryRow("SELECT state FROM routine_runs WHERE id=?", record.ID).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if runState != record.State {
		t.Fatalf("run index state %q differs from receipt %q", runState, record.State)
	}
	rows, err := r.db.Query("SELECT node_id,binding_id,task_id,state,body FROM routine_steps WHERE run_id=?", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var indexed routineStep
		var body string
		if err = rows.Scan(&indexed.NodeID, &indexed.BindingID, &indexed.TaskID, &indexed.State, &body); err != nil {
			t.Fatal(err)
		}
		var stored routineStep
		if err = json.Unmarshal([]byte(body), &stored); err != nil {
			t.Fatal(err)
		}
		expected := routineStepByNode(record.Steps, indexed.NodeID)
		if expected == nil || stored != *expected || indexed.BindingID != stored.BindingID || indexed.TaskID != stored.TaskID || indexed.State != stored.State {
			t.Fatalf("step index differs from run receipt: indexed=%+v body=%+v expected=%+v", indexed, stored, expected)
		}
		seen[indexed.NodeID] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(record.Steps) {
		t.Fatalf("indexed %d steps for %d reservations", len(seen), len(record.Steps))
	}
}

func TestRoutineDueOccurrenceClaimsExactlyOneRun(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Scheduler profile")
	routine := routineSave(t, r, map[string]any{
		"name": "Interval routine", "profileId": profile.ID, "instruction": "Summarize the workspace.",
		"trigger": map[string]any{"kind": "interval", "everySeconds": 60}, "enabled": true, "autoStart": true, "timeoutSeconds": 300,
	})
	if !routine.Enabled || routine.Revision != 1 || routine.NextDueAt == "" {
		t.Fatalf("unexpected routine view: %+v", routine)
	}
	routineDue(t, r, routine.ID, now().Add(-30*time.Second))
	r.mu.Lock()
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected exactly one claimed occurrence, got %d", len(claimed))
	}
	r.mu.Lock()
	again, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("the same occurrence was claimed twice: %v", again)
	}
	runs := routineRunsOf(t, r, routine.ID)
	if len(runs) != 1 || runs[0].State != routineRunClaimed || runs[0].ScheduledAt == "" {
		t.Fatalf("unexpected run receipts: %+v", runs)
	}
}

func TestRoutineCronAndOnceSchedulesClaimOneOccurrence(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Cron profile")
	cron := routineSave(t, r, map[string]any{
		"name": "Cron routine", "profileId": profile.ID, "instruction": "Review the inbox.",
		"trigger": map[string]any{"kind": "cron", "expression": "*/5 * * * *", "timeZone": "UTC"}, "enabled": true, "autoStart": true,
	})
	once := routineSave(t, r, map[string]any{
		"name": "One shot routine", "profileId": profile.ID, "instruction": "Archive the summary.",
		"trigger": map[string]any{"kind": "once", "at": now().Add(time.Hour).Format(time.RFC3339)}, "enabled": true, "autoStart": true,
	})
	// The cron occurrence is the last scheduled instant at or before now, so
	// exactly one occurrence is due.
	schedule, err := cronlib.ParseStandard("*/5 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	cursor := schedule.Next(now().Add(-10 * time.Minute))
	for {
		next := schedule.Next(cursor)
		if next.After(now()) {
			break
		}
		cursor = next
	}
	routineDue(t, r, cron.ID, cursor)
	routineDue(t, r, once.ID, now().Add(-time.Minute))
	r.mu.Lock()
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("expected one occurrence per routine, got %d", len(claimed))
	}
	if runs := routineRunsOf(t, r, cron.ID); len(runs) != 1 {
		t.Fatalf("cron routine produced %d receipts", len(runs))
	}
	if runs := routineRunsOf(t, r, once.ID); len(runs) != 1 {
		t.Fatalf("one-shot routine produced %d receipts", len(runs))
	}
	// A one-shot routine disables itself after its single occurrence.
	r.mu.Lock()
	var enabled, nextDue string
	if err = r.db.QueryRow("SELECT enabled,next_due_at FROM routines WHERE id=?", once.ID).Scan(&enabled, &nextDue); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.mu.Unlock()
	if enabled != "0" || nextDue != "" {
		t.Fatalf("one-shot routine stayed scheduled: %q %q", enabled, nextDue)
	}
}

func TestRoutineMissedOccurrenceIsSkippedWithVisibleReason(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Backlog profile")
	routine := routineSave(t, r, map[string]any{
		"name": "Backlog routine", "profileId": profile.ID, "instruction": "Collect the daily summary.",
		"trigger": map[string]any{"kind": "interval", "everySeconds": 60}, "enabled": true, "autoStart": true,
	})
	routineDue(t, r, routine.ID, now().Add(-5*time.Minute))
	r.mu.Lock()
	_, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	runs := routineRunsOf(t, r, routine.ID)
	skipped := 0
	claimed := 0
	for _, run := range runs {
		switch run.State {
		case routineRunSkipped:
			skipped++
			if run.Reason == "" || run.SkippedCount == 0 {
				t.Fatalf("skipped occurrence has no visible reason: %+v", run)
			}
		case routineRunClaimed:
			claimed++
		}
	}
	if skipped != 1 || claimed != 1 {
		t.Fatalf("expected one skipped backlog and one claimed occurrence, got %+v", runs)
	}
}

func TestRoutineDowntimeAdvancesMissedOccurrencesWithoutReplay(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Downtime profile")
	routine := routineSave(t, r, map[string]any{
		"name": "Downtime routine", "profileId": profile.ID, "instruction": "Collect the daily summary.",
		"trigger": map[string]any{"kind": "interval", "everySeconds": 60}, "enabled": true, "autoStart": true,
	})
	routineDue(t, r, routine.ID, now().Add(-5*time.Minute))
	r.routineRecoverDowntime()
	runs := routineRunsOf(t, r, routine.ID)
	if len(runs) != 1 || runs[0].State != routineRunSkipped || runs[0].Reason != routineReasonDowntime {
		t.Fatalf("downtime replayed a missed occurrence: %+v", runs)
	}
	if runs[0].SkippedCount != 6 {
		t.Fatalf("downtime did not report the skipped count: %+v", runs[0])
	}
	r.mu.Lock()
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("downtime left a claimable occurrence: %v", claimed)
	}
}

func TestRoutineOverlappingOccurrenceIsSkipped(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Overlap profile")
	routine := routineSave(t, r, map[string]any{
		"name": "Overlap routine", "profileId": profile.ID, "instruction": "Produce the report.",
		"trigger": map[string]any{"kind": "interval", "everySeconds": 60}, "enabled": true, "autoStart": true,
	})
	active := routineRunRecord{ID: randomID(), RoutineID: routine.ID, Revision: routine.Revision, ScheduledAt: routineTime(now().Add(-time.Hour)), Source: routineSourceSchedule, State: routineRunRunning, Steps: []routineStep{}, TaskIDs: []string{}, CreatedAt: routineTime(now()), UpdatedAt: routineTime(now())}
	body, err := json.Marshal(active)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	_, err = r.db.Exec("INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,?,?,?,?,?,?)", active.ID, active.RoutineID, active.Revision, active.ScheduledAt, active.Source, active.State, string(body))
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	routineDue(t, r, routine.ID, now().Add(-30*time.Second))
	r.mu.Lock()
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("an overlapping occurrence was claimed: %v", claimed)
	}
	runs := routineRunsOf(t, r, routine.ID)
	found := false
	for _, run := range runs {
		if run.State == routineRunSkipped && run.Reason == routineReasonOverlap {
			found = true
		}
	}
	if !found {
		t.Fatalf("the overlapping occurrence has no visible reason: %+v", runs)
	}
}

func TestRoutineScheduledRunWithoutAutoStartIsSkipped(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Auto start profile")
	routine := routineSave(t, r, map[string]any{
		"name": "No auto start", "profileId": profile.ID, "instruction": "Produce the report.",
		"trigger": map[string]any{"kind": "interval", "everySeconds": 60}, "enabled": true, "autoStart": false,
	})
	routineDue(t, r, routine.ID, now().Add(-30*time.Second))
	r.mu.Lock()
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("a stopped binding was started without autoStart: %v", claimed)
	}
	runs := routineRunsOf(t, r, routine.ID)
	if len(runs) != 1 || runs[0].State != routineRunSkipped || runs[0].Reason != routineReasonAutoStart {
		t.Fatalf("the blocked occurrence has no visible reason: %+v", runs)
	}
}

func TestRoutineWebhookSignatureReplayAndConflict(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Webhook profile")
	view := routineSave(t, r, map[string]any{
		"name": "Webhook routine", "profileId": profile.ID, "instruction": "Handle the incoming event.",
		"trigger": map[string]any{"kind": "webhook"}, "enabled": true, "autoStart": true,
	})
	if view.WebhookSecret == "" || view.WebhookSecretStatus != "saved" {
		t.Fatalf("webhook routine has no secret: %+v", view)
	}
	body := []byte(`{"event":"ping"}`)
	timestamp := fmtTime(now())
	eventID := "event-1"
	signature := routineSign(view.WebhookSecret, timestamp, eventID, body)
	value, err := r.RoutineWebhook(context.Background(), view.ID, timestamp, eventID, "sha256="+signature, body)
	if err != nil {
		t.Fatal(err)
	}
	first := value.(routineRunRecord)
	if first.State != routineRunRunning || first.Source != routineSourceWebhook {
		t.Fatalf("unexpected webhook receipt: %+v", first)
	}
	// The same event returns the existing receipt instead of a second run.
	value, err = r.RoutineWebhook(context.Background(), view.ID, timestamp, eventID, "sha256="+signature, body)
	if err != nil {
		t.Fatal(err)
	}
	if value.(routineRunRecord).ID != first.ID {
		t.Fatalf("a replayed event created a second run: %+v", value)
	}
	// The same event id with a different payload conflicts.
	other := []byte(`{"event":"pong"}`)
	conflict := routineSign(view.WebhookSecret, timestamp, eventID, other)
	_, err = r.RoutineWebhook(context.Background(), view.ID, timestamp, eventID, "sha256="+conflict, other)
	requireStatus(t, err, 409)
	// An invalid signature is refused.
	_, err = r.RoutineWebhook(context.Background(), view.ID, timestamp, "event-2", "sha256="+strings.Repeat("0", 64), body)
	requireStatus(t, err, 401)
	// A stale timestamp is refused.
	_, err = r.RoutineWebhook(context.Background(), view.ID, fmtTime(now().Add(-10*time.Minute)), "event-3", "sha256="+signature, body)
	requireStatus(t, err, 401)
	if runs := routineRunsOf(t, r, view.ID); len(runs) != 1 {
		t.Fatalf("webhook produced %d receipts", len(runs))
	}
}

func TestRoutineTokenRunsOnlyGrantedRoutines(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Token profile")
	granted := routineSave(t, r, map[string]any{
		"name": "Granted routine", "profileId": profile.ID, "instruction": "Produce the report.",
		"trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": true,
	})
	denied := routineSave(t, r, map[string]any{
		"name": "Denied routine", "profileId": profile.ID, "instruction": "Produce another report.",
		"trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": true,
	})
	value, err := routineCall(t, r, "routineTokens.create", map[string]any{"routineIds": []string{granted.ID}})
	if err != nil {
		t.Fatal(err)
	}
	token := value.(routineTokenView)
	if token.Token == "" || token.ID == "" {
		t.Fatalf("token was not issued once: %+v", token)
	}
	principal, err := r.AuthenticateRoutineToken(token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Admin || principal.AgentID != "" || principal.SessionID != "" || principal.TaskID != "" || principal.ConsoleSessionID != "" || principal.RoutineTokenID != token.ID {
		t.Fatalf("routine credential is not a narrow principal: %+v", principal)
	}
	// Only routines.run is reachable with a routine credential.
	_, err = r.routineOperation(context.Background(), principal, "routines.list", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
	_, err = r.Agents(principal)
	requireStatus(t, err, 403)
	_, err = r.Automation(context.Background(), principal, "profiles.list", json.RawMessage(`{}`))
	requireStatus(t, err, 403)
	_, err = r.routineOperation(context.Background(), principal, "routines.save", json.RawMessage(`{"routine":{"name":"x"}}`))
	requireStatus(t, err, 403)
	// A routine that the token does not grant is refused.
	_, err = r.routineOperation(context.Background(), principal, "routines.run", json.RawMessage(`{"id":"`+denied.ID+`","idempotencyKey":"key-1"}`))
	requireStatus(t, err, 403)
	// The granted routine runs.
	value, err = r.routineOperation(context.Background(), principal, "routines.run", json.RawMessage(`{"id":"`+granted.ID+`","idempotencyKey":"key-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	if run.State != routineRunRunning || run.Source != routineSourceToken {
		t.Fatalf("unexpected token run: %+v", run)
	}
	// The same idempotency key returns the same receipt.
	value, err = r.routineOperation(context.Background(), principal, "routines.run", json.RawMessage(`{"id":"`+granted.ID+`","idempotencyKey":"key-2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if value.(routineRunRecord).ID != run.ID {
		t.Fatalf("the idempotency key created a second run: %+v", value)
	}
	// Revocation blocks new admissions.
	if _, err = routineCall(t, r, "routineTokens.revoke", map[string]any{"id": token.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = r.AuthenticateRoutineToken(token.Token)
	requireStatus(t, err, 401)
	// Expiry blocks new admissions.
	value, err = routineCall(t, r, "routineTokens.create", map[string]any{"routineIds": []string{granted.ID}, "expiresAt": now().Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	expiring := value.(routineTokenView)
	r.mu.Lock()
	_, err = r.db.Exec("UPDATE routine_execution_tokens SET expires_at=? WHERE id=?", routineTime(now().Add(-time.Minute)), expiring.ID)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.AuthenticateRoutineToken(expiring.Token)
	requireStatus(t, err, 401)
}

func TestRoutineFlowRunsDownstreamAfterSuccessAndSkipsAfterFailure(t *testing.T) {
	failing := true
	r, _ := routineOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		if failing && strings.Contains(turn.Task.Text, "FAIL") {
			return "", errors.New("model failure")
		}
		return "complete", nil
	})
	first := routineProfile(t, r, "Flow first")
	second := routineProfile(t, r, "Flow second")
	flowValue, err := routineCall(t, r, "flows.save", map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "profileId": first.ID, "instruction": "FAIL collect the inputs"},
			{"id": "b", "profileId": second.ID, "instruction": "Write the summary"},
		},
		"edges": []map[string]any{{"from": "a", "to": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	flow := flowValue.(routineFlow)
	if flow.ID == "" || flow.Revision != 1 || len(flow.Nodes) != 2 || len(flow.Edges) != 1 {
		t.Fatalf("unexpected flow view: %+v", flow)
	}
	routine := routineSave(t, r, map[string]any{
		"name": "Flow routine", "profileId": first.ID, "instruction": "Collect the inputs",
		"trigger": map[string]any{"kind": "manual"},
		"flowId":  flow.ID, "flowRevision": flow.Revision, "enabled": true, "autoStart": true,
	})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "flow-run"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	if run.State != routineRunRunning || len(run.Steps) != 2 {
		t.Fatalf("unexpected flow run: %+v", run)
	}
	if run.Steps[1].State != routineStepReserved || run.Steps[1].TaskID != "" {
		t.Fatalf("the downstream node was not reserved: %+v", run.Steps)
	}
	var rootTask string
	for _, step := range run.Steps {
		if step.NodeID == "a" {
			rootTask = step.TaskID
		}
	}
	if rootTask == "" {
		t.Fatalf("the root node has no task: %+v", run.Steps)
	}
	awaitTask(t, r, rootTask)
	routineReconcile(t, r)
	runs := routineRunsOf(t, r, routine.ID)
	failed := runs[len(runs)-1]
	states := map[string]routineStep{}
	for _, step := range failed.Steps {
		states[step.NodeID] = step
	}
	if states["a"].State != routineStepFailed {
		t.Fatalf("the failing node is not recorded: %+v", failed.Steps)
	}
	if states["b"].State != routineStepSkipped || states["b"].Reason != routineReasonDependency {
		t.Fatalf("the dependent node was not skipped: %+v", failed.Steps)
	}
	if failed.State != routineRunFailed {
		t.Fatalf("the run did not settle as failed: %+v", failed)
	}
	routineAssertStepIndex(t, r, failed)
}

func TestRoutineFlowRunsDownstreamAfterParentSuccess(t *testing.T) {
	releaseDownstream := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseDownstream:
		default:
			close(releaseDownstream)
		}
	})
	r, _ := routineOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		if strings.Contains(turn.Task.Text, "Write the summary") {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-releaseDownstream:
			}
		}
		return "complete", nil
	})
	r.mu.Lock()
	r.settings.MaxStartedAgents = 1
	r.mu.Unlock()
	first := routineProfile(t, r, "Success first")
	second := routineProfile(t, r, "Success second")
	flowValue, err := routineCall(t, r, "flows.save", map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "profileId": first.ID, "instruction": "Collect the inputs"},
			{"id": "b", "profileId": second.ID, "instruction": "Write the summary"},
		},
		"edges": []map[string]any{{"from": "a", "to": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	flow := flowValue.(routineFlow)
	routine := routineSave(t, r, map[string]any{
		"name": "Success flow routine", "profileId": first.ID, "instruction": "Collect the inputs",
		"trigger": map[string]any{"kind": "manual"},
		"flowId":  flow.ID, "flowRevision": flow.Revision, "enabled": true, "autoStart": true,
	})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "success-run"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	var rootTask string
	for _, step := range run.Steps {
		if step.NodeID == "a" {
			rootTask = step.TaskID
		}
	}
	awaitTask(t, r, rootTask)
	routineReconcile(t, r)
	runs := routineRunsOf(t, r, routine.ID)
	settled := runs[len(runs)-1]
	states := map[string]routineStep{}
	for _, step := range settled.Steps {
		states[step.NodeID] = step
	}
	if states["a"].State != routineStepCompleted {
		t.Fatalf("the parent node is not completed: %+v", settled.Steps)
	}
	if states["b"].State != routineStepAdmitted || states["b"].TaskID == "" {
		t.Fatalf("the dependent node was not admitted: %+v", settled.Steps)
	}
	routineAssertStepIndex(t, r, settled)
	child, err := r.Task(admin, states["b"].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentTaskID != "" || !strings.Contains(child.Text, "complete") {
		t.Fatalf("downstream handoff lost or ancestry forged: %+v", child)
	}
	close(releaseDownstream)
	child = awaitTask(t, r, child.ID)
	if child.State != "completed" {
		t.Fatalf("downstream did not finish within the one-started-agent limit: %+v", child)
	}
	routineReconcile(t, r)
	runs = routineRunsOf(t, r, routine.ID)
	settled = runs[len(runs)-1]
	if settled.State != routineRunCompleted || !settled.BindingsReleased || len(settled.TaskIDs) != 2 {
		t.Fatalf("completed flow did not durably release its bindings: %+v", settled)
	}
	for _, step := range settled.Steps {
		if step.State != routineStepCompleted {
			t.Fatalf("terminal flow retained a nonterminal step: %+v", settled.Steps)
		}
		r.mu.Lock()
		binding, bindingErr := r.routineBindingByID(step.BindingID)
		started := r.slots[binding.AgentID] != nil
		r.mu.Unlock()
		if bindingErr != nil || started {
			t.Fatalf("settled binding retained a native runtime: %+v, %v", binding, bindingErr)
		}
	}
	routineAssertStepIndex(t, r, settled)
	routineReconcile(t, r)
	if runs = routineRunsOf(t, r, routine.ID); len(runs) != 1 || len(runs[0].TaskIDs) != 2 {
		t.Fatalf("reconciliation replayed completed work: %+v", runs)
	}
}

func routineTestFlow(t *testing.T, r *Relay, edges []routineFlowEdge) routineView {
	t.Helper()
	first := routineProfile(t, r, "First flow profile")
	second := routineProfile(t, r, "Second flow profile")
	value, err := routineCall(t, r, "flows.save", map[string]any{
		"nodes": []routineFlowNode{
			{ID: "a", ProfileID: first.ID, Instruction: "Collect the inputs"},
			{ID: "b", ProfileID: second.ID, Instruction: "Write the summary"},
		},
		"edges": edges,
	})
	if err != nil {
		t.Fatal(err)
	}
	flow := value.(routineFlow)
	return routineSave(t, r, map[string]any{
		"name": "Reviewed flow", "profileId": first.ID, "instruction": "Collect the inputs",
		"trigger": map[string]any{"kind": "manual"}, "flowId": flow.ID, "flowRevision": flow.Revision,
		"enabled": true, "autoStart": false, "timeoutSeconds": 300,
	})
}

func TestRoutineFlowCancelStopsNativeTurnAndReleasesReservations(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	r, _ := routineOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	})
	routine := routineTestFlow(t, r, []routineFlowEdge{{From: "a", To: "b"}})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "cancel-running"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("root native turn did not start")
	}
	value, err = routineCall(t, r, "routines.cancelRun", map[string]any{"runId": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelRun did not cancel the active native turn")
	}
	runs := routineRunsOf(t, r, routine.ID)
	run = runs[0]
	if run.State != routineRunCancelled || !run.BindingsReleased {
		t.Fatalf("cancelRun left native bindings owned: %+v", run)
	}
	for _, step := range run.Steps {
		if step.State != routineStepCancelled {
			t.Fatalf("cancelRun retained an outstanding reservation: %+v", step)
		}
	}
	task, err := r.Task(admin, run.TaskIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "cancelled" || !task.Ambiguous || task.ErrorCategory != "cancelled" {
		t.Fatalf("cancelRun lost native execution uncertainty: %+v", task)
	}
	routineAssertStepIndex(t, r, run)
	routineReconcile(t, r)
	value, err = routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "cancel-running"})
	if err != nil || value.(routineRunRecord).ID != run.ID || len(value.(routineRunRecord).TaskIDs) != 1 {
		t.Fatalf("cancelled occurrence was replayed: value=%+v err=%v", value, err)
	}
}

func TestRoutineFlowCancellationPreservesSettledParentAndRecoversCleanup(t *testing.T) {
	finish := make(chan struct{})
	started := make(chan struct{})
	r, _ := routineOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		close(started)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-finish:
			return "parent finished", nil
		}
	})
	routine := routineTestFlow(t, r, []routineFlowEdge{{From: "a", To: "b"}})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "cancel-after-parent"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("root native turn did not start")
	}
	// Keep the completion hook from reconciling before cancellation, and then
	// leave cleanup pending as if the daemon exited after persisting the receipt.
	r.lifecycle.Lock()
	close(finish)
	parent := awaitTask(t, r, run.TaskIDs[0])
	r.mu.Lock()
	run, err = r.cancelRoutineRunLocked(run.ID)
	r.mu.Unlock()
	r.lifecycle.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if parent.State != "completed" || run.BindingsReleased {
		t.Fatalf("unexpected pre-cleanup state: task=%+v run=%+v", parent, run)
	}
	if routineStepByNode(run.Steps, "a").State != routineStepCompleted || routineStepByNode(run.Steps, "b").State != routineStepCancelled {
		t.Fatalf("cancellation overwrote a completed parent or retained a reservation: %+v", run.Steps)
	}
	routineAssertStepIndex(t, r, run)
	routineReconcile(t, r)
	runs := routineRunsOf(t, r, routine.ID)
	if len(runs) != 1 || !runs[0].BindingsReleased || len(runs[0].TaskIDs) != 1 || routineStepByNode(runs[0].Steps, "b").TaskID != "" {
		t.Fatalf("terminal cleanup did not recover without replay: %+v", runs)
	}
}

func TestRoutineFlowAdmissionFailureReleasesEveryReservation(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	routine := routineTestFlow(t, r, []routineFlowEdge{})
	r.mu.Lock()
	_, err := r.db.Exec("CREATE TRIGGER routine_test_fail_second_node BEFORE INSERT ON routine_steps WHEN NEW.node_id='b' AND NEW.task_id<>'' BEGIN SELECT RAISE(ABORT,'test second-node persistence failure'); END")
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "partial-admission"})
	if err == nil {
		t.Fatal("second-node transaction unexpectedly succeeded")
	}
	runs := routineRunsOf(t, r, routine.ID)
	if len(runs) != 1 {
		t.Fatalf("partial admission lost its durable receipt: %+v", runs)
	}
	run := runs[0]
	if run.State != routineRunFailed || !run.BindingsReleased || len(run.TaskIDs) != 1 || !routineRunStepsSettled(run.Steps) {
		t.Fatalf("partial admission retained outstanding work: %+v", run)
	}
	if routineStepByNode(run.Steps, "a").State != routineStepCancelled || routineStepByNode(run.Steps, "b").State != routineStepSkipped {
		t.Fatalf("partial admission did not settle every reservation: %+v", run.Steps)
	}
	routineAssertStepIndex(t, r, run)
	r.mu.Lock()
	var taskCount int
	err = r.db.QueryRow("SELECT count(*) FROM tasks WHERE json_extract(body,'$.conversationId')=?", run.ConversationID).Scan(&taskCount)
	r.mu.Unlock()
	if err != nil || taskCount != 1 {
		t.Fatalf("failed task transaction left an unreceipted task: count=%d err=%v", taskCount, err)
	}
	routineReconcile(t, r)
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "partial-admission"})
	if err != nil || value.(routineRunRecord).ID != run.ID || len(value.(routineRunRecord).TaskIDs) != 1 {
		t.Fatalf("failed occurrence was replayed: value=%+v err=%v", value, err)
	}
}

func TestRoutineClaimRecoveryHonorsAutoStartAndRevision(t *testing.T) {
	for _, source := range []string{routineSourceSchedule, routineSourceToken, routineSourceWebhook, routineSourceManual} {
		t.Run(source, func(t *testing.T) {
			r, _ := routineOpenTest(t, nil)
			profile := routineProfile(t, r, "Recovery profile")
			view := routineSave(t, r, map[string]any{
				"name": "Recovery routine", "profileId": profile.ID, "instruction": "Collect the inputs",
				"trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": false,
			})
			r.mu.Lock()
			d, _, err := r.routineDefinition(view.ID)
			if err == nil {
				_, err = r.createRoutineRun(d, source, now(), routineRunClaimed, "", "")
			}
			r.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			expected := routineRunSkipped
			if source == routineSourceManual {
				expected = routineRunFailed
				if _, err = routineCall(t, r, "routines.save", map[string]any{"routine": map[string]any{"id": view.ID, "instruction": "Changed reviewed work"}, "expectedRevision": view.Revision}); err != nil {
					t.Fatal(err)
				}
			}
			routineReconcile(t, r)
			runs := routineRunsOf(t, r, view.ID)
			if len(runs) != 1 || runs[0].State != expected || len(runs[0].TaskIDs) != 0 || runs[0].ConversationID != "" {
				t.Fatalf("recovery widened startup authority or adopted a new revision: %+v", runs)
			}
		})
	}
}

func TestRoutineFlowCycleAndDuplicateNodesAreRejected(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	first := routineProfile(t, r, "Cycle first")
	second := routineProfile(t, r, "Cycle second")
	_, err := routineCall(t, r, "flows.save", map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "profileId": first.ID, "instruction": "Collect"},
			{"id": "b", "profileId": second.ID, "instruction": "Summarize"},
		},
		"edges": []map[string]any{{"from": "a", "to": "b"}, {"from": "b", "to": "a"}},
	})
	requireStatus(t, err, 400)
	_, err = routineCall(t, r, "flows.save", map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "profileId": first.ID, "instruction": "Collect"},
			{"id": "a", "profileId": second.ID, "instruction": "Summarize"},
		},
		"edges": []map[string]any{},
	})
	requireStatus(t, err, 400)
	_, err = routineCall(t, r, "flows.save", map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "profileId": first.ID, "instruction": "Collect"},
		},
		"edges": []map[string]any{{"from": "a", "to": "missing"}},
	})
	requireStatus(t, err, 400)
}

func TestRoutineOperationsRequireAdministrator(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Operator profile")
	routineSave(t, r, map[string]any{
		"name": "Operator routine", "profileId": profile.ID, "instruction": "Produce the report.",
		"trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": true,
	})
	principal := model.Principal{AgentID: "omp:unprivileged"}
	for _, operation := range []string{"routines.list", "routines.save", "routines.preview", "routines.runs", "flows.get", "routineTokens.list"} {
		body, err := json.Marshal(map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.routineOperation(context.Background(), principal, operation, body); err == nil {
			t.Fatalf("%s accepted a non-administrator", operation)
		} else {
			requireStatus(t, err, 403)
		}
	}
}

func TestRoutineTriggerValidationRejectsUnsafeSchedules(t *testing.T) {
	r, _ := routineOpenTest(t, nil)
	profile := routineProfile(t, r, "Schedule profile")
	for _, trigger := range []map[string]any{
		{"kind": "cron", "expression": "@daily"},
		{"kind": "cron", "expression": "0 0 * * * *"},
		{"kind": "interval", "everySeconds": 5},
		{"kind": "once"},
		{"kind": "unknown"},
	} {
		_, err := routineCall(t, r, "routines.save", map[string]any{"routine": map[string]any{
			"name": "Unsafe schedule", "profileId": profile.ID, "instruction": "Produce the report.",
			"trigger": trigger, "enabled": true,
		}})
		requireStatus(t, err, 400)
	}
}

func fmtTime(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func routineSign(secret, timestamp, eventID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + eventID + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestDeferredFlowReservationBlocksPolicyWideningUntilCancel(t *testing.T) {
	started := make(chan struct{})
	r, _ := routineOpenTest(t, func(ctx context.Context, turn model.Turn, emit model.Emit) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	first := routineProfile(t, r, "Reserved flow first")
	second := routineProfile(t, r, "Reserved flow second")
	flowValue, err := routineCall(t, r, "flows.save", map[string]any{
		"nodes": []routineFlowNode{{ID: "a", ProfileID: first.ID, Instruction: "Wait"}, {ID: "b", ProfileID: second.ID, Instruction: "Deferred"}},
		"edges": []routineFlowEdge{{From: "a", To: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	flow := flowValue.(routineFlow)
	routine := routineSave(t, r, map[string]any{"name": "Reserved permissions", "profileId": first.ID, "instruction": "Wait", "trigger": map[string]any{"kind": "manual"}, "flowId": flow.ID, "flowRevision": flow.Revision, "enabled": true, "autoStart": true})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "reservation"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("root did not start")
	}
	request := map[string]any{"id": second.ID, "expectedRevision": second.Revision, "name": second.Name, "instructions": second.Instructions, "workspaceId": second.WorkspaceID, "binding": second.Binding, "capabilities": model.ProfileCapabilities{MemoryWrite: true}, "allowedProfileIds": []string{}}
	_, err = profileStoreCall(t, r, "profiles.save", request)
	requireStatus(t, err, 409)
	if _, err = routineCall(t, r, "routines.cancelRun", map[string]any{"runId": run.ID}); err != nil {
		t.Fatal(err)
	}
	value, err = profileStoreCall(t, r, "profiles.save", request)
	if err != nil {
		t.Fatal(err)
	}
	updated := value.(model.Profile)
	if !updated.Capabilities.MemoryWrite {
		t.Fatal("cancelled reservation retained a policy hold")
	}
}

func TestRoutinePromptExcludesPrivateOperatorMemory(t *testing.T) {
	captured := make(chan string, 1)
	r, _ := routineOpenTest(t, func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
		captured <- turn.Task.Text
		return "routine finished", nil
	})
	profile := routineProfile(t, r, "Scoped routine memory")
	canary := "PRIVATE-OPERATOR-MEMORY-6da6814b"
	body, err := json.Marshal(map[string]any{"profileId": profile.ID, "scope": "operator", "content": canary, "pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.profileMemoryOperation(context.Background(), admin, "memory.save", body); err != nil {
		t.Fatal(err)
	}
	routine := routineSave(t, r, map[string]any{"name": "Scoped memory", "profileId": profile.ID, "instruction": "Use only the isolated run context.", "trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": true})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "private-memory-scope"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	awaitTask(t, r, run.TaskIDs[0])
	select {
	case prompt := <-captured:
		if strings.Contains(prompt, canary) {
			t.Fatal("routine received operator-only memory from a private profile conversation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("routine did not receive its admitted prompt")
	}
}

func TestAutomaticConversationMemoryScopeSurvivesFollowupRetentionAndSwitch(t *testing.T) {
	prompts := make(chan string, 3)
	r, _ := routineOpenTest(t, func(_ context.Context, turn model.Turn, _ model.Emit) (string, error) {
		prompts <- turn.Task.Text
		return "done", nil
	})
	profile := routineProfile(t, r, "Persistent automatic scope")
	canary := "PRIVATE-OPERATOR-MEMORY-durable-audience"
	body, _ := json.Marshal(map[string]any{"profileId": profile.ID, "scope": "operator", "content": canary, "pinned": true})
	if _, err := r.profileMemoryOperation(context.Background(), admin, "memory.save", body); err != nil {
		t.Fatal(err)
	}
	routine := routineSave(t, r, map[string]any{"name": "Automatic audience", "profileId": profile.ID, "instruction": "Use only this run's scoped context.", "trigger": map[string]any{"kind": "manual"}, "enabled": true, "autoStart": true})
	value, err := routineCall(t, r, "routines.run", map[string]any{"id": routine.ID, "idempotencyKey": "durable-audience"})
	if err != nil {
		t.Fatal(err)
	}
	run := value.(routineRunRecord)
	first := awaitTask(t, r, run.TaskIDs[0])
	deadline := time.Now().Add(3 * time.Second)
	for {
		agent, err := r.Agent(admin, first.To)
		if err != nil {
			t.Fatal(err)
		}
		if agent.State == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal routine binding did not release")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if prompt := <-prompts; strings.Contains(prompt, canary) {
		t.Fatal("initial routine leaked private memory")
	}
	conversation := model.Conversation{ID: run.ConversationID}
	followup := continuitySend(t, r, profile, conversation, "Continue within the automatic conversation")
	if completed := awaitTask(t, r, followup.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	if prompt := <-prompts; strings.Contains(prompt, canary) {
		t.Fatal("local follow-up widened automatic-conversation memory scope")
	}
	r.mu.Lock()
	_, err = r.db.Exec("DELETE FROM routine_trigger_receipts WHERE run_id=?", run.ID)
	if err == nil {
		_, err = r.db.Exec("DELETE FROM routine_runs WHERE id=?", run.ID)
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	switched, err := continuityCall(t, r, "profiles.switch", map[string]any{"id": profile.ID, "expectedRevision": profile.Revision, "binding": model.ProfileProvider{Harness: "omp"}, "shareHistory": true})
	if err != nil {
		t.Fatal(err)
	}
	profile = switched.(model.Profile)
	retained := continuitySend(t, r, profile, conversation, "Continue after receipt pruning and provider switch")
	if completed := awaitTask(t, r, retained.ID); completed.State != "completed" {
		t.Fatal(completed.Error)
	}
	if prompt := <-prompts; strings.Contains(prompt, canary) {
		t.Fatal("retention or provider switch erased automatic-conversation memory scope")
	}
}
