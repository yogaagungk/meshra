package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"meshra/internal/model"
)

// Durable routines and forms-based flows. A routine is an operator-authored
// definition: the trigger (schedule, webhook or execution token) only selects
// when an operator-approved instruction runs. A trigger never becomes the task
// sender and never gains administrative authority.

const (
	routineTimeLayout      = "2006-01-02T15:04:05.000000000Z"
	routineMaxInstruction  = 64 * 1024
	routineMaxNameBytes    = 128
	routineMinInterval     = 60
	routineMaxTimeout      = 86400
	routineDefaultTimeout  = 3600
	routineMaxPreview      = 10
	routinePageLimit       = 100
	routineDefaultPage     = 50
	routineWebhookMaxBody  = 64 * 1024
	routineWebhookWindow   = 5 * time.Minute
	routineMaxEventIDBytes = 200
	routineScanLimit       = 32
)

// Run states are distinct from task states; a run is a receipt, not a task.
const (
	routineRunClaimed   = "claimed"
	routineRunRunning   = "running"
	routineRunCompleted = "completed"
	routineRunFailed    = "failed"
	routineRunCancelled = "cancelled"
	routineRunSkipped   = "skipped"
)

// Step states track one node of a run.
const (
	routineStepReserved  = "reserved"
	routineStepAdmitted  = "admitted"
	routineStepCompleted = "completed"
	routineStepFailed    = "failed"
	routineStepCancelled = "cancelled"
	routineStepSkipped   = "skipped"
)

// Visible reasons for a skipped or failed occurrence.
const (
	routineReasonOverlap    = "overlap"
	routineReasonBacklog    = "backlog"
	routineReasonDowntime   = "downtime"
	routineReasonAutoStart  = "auto_start_disabled"
	routineReasonDependency = "skipped_dependency"
	routineReasonSuperseded = "binding_superseded"
)

// Sources that may trigger an occurrence.
const (
	routineSourceSchedule = "schedule"
	routineSourceManual   = "manual"
	routineSourceWebhook  = "webhook"
	routineSourceToken    = "token"
)

var errRoutineNoOccurrence = errors.New("routine has no further occurrence")

type routineTrigger struct {
	Kind         string `json:"kind"`
	At           string `json:"at,omitempty"`
	EverySeconds int    `json:"everySeconds,omitempty"`
	Expression   string `json:"expression,omitempty"`
	TimeZone     string `json:"timeZone,omitempty"`
}

type routineDefinition struct {
	ID             string         `json:"id"`
	Revision       int            `json:"revision"`
	Name           string         `json:"name"`
	ProfileID      string         `json:"profileId"`
	Instruction    string         `json:"instruction"`
	Trigger        routineTrigger `json:"trigger"`
	FlowID         string         `json:"flowId,omitempty"`
	FlowRevision   int            `json:"flowRevision,omitempty"`
	Enabled        bool           `json:"enabled"`
	AutoStart      bool           `json:"autoStart"`
	TimeoutSeconds int            `json:"timeoutSeconds"`
	// WebhookSecretRef points at the encrypted routine webhook key. The
	// plaintext is never stored in the routine record.
	WebhookSecretRef string `json:"webhookSecretRef,omitempty"`
}

// routineInput is the operator supplied definition. Absent fields keep the
// stored value; the webhook secret is a write-only plaintext input.
type routineInput struct {
	Trigger        *routineTrigger `json:"trigger,omitempty"`
	Enabled        *bool           `json:"enabled,omitempty"`
	AutoStart      *bool           `json:"autoStart,omitempty"`
	FlowID         *string         `json:"flowId,omitempty"`
	WebhookSecret  string          `json:"webhookSecret,omitempty"`
	Name           string          `json:"name,omitempty"`
	ProfileID      string          `json:"profileId,omitempty"`
	Instruction    string          `json:"instruction,omitempty"`
	FlowRevision   int             `json:"flowRevision,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
	ID             string          `json:"id,omitempty"`
	Revision       int             `json:"revision,omitempty"`
}

type routineView struct {
	ID             string         `json:"id"`
	Revision       int            `json:"revision"`
	Name           string         `json:"name"`
	ProfileID      string         `json:"profileId"`
	Instruction    string         `json:"instruction"`
	Trigger        routineTrigger `json:"trigger"`
	FlowID         string         `json:"flowId,omitempty"`
	FlowRevision   int            `json:"flowRevision,omitempty"`
	Enabled        bool           `json:"enabled"`
	AutoStart      bool           `json:"autoStart"`
	TimeoutSeconds int            `json:"timeoutSeconds"`
	NextDueAt      string         `json:"nextDueAt,omitempty"`
	// WebhookSecretStatus reports missing, saved or unreadable without
	// exposing any part of the secret.
	WebhookSecretStatus string `json:"webhookSecretStatus"`
	// WebhookSecret is populated only by the create response of a webhook
	// routine that generated its own secret.
	WebhookSecret string `json:"webhookSecret,omitempty"`
}

type routineStep struct {
	NodeID    string `json:"nodeId"`
	BindingID string `json:"bindingId"`
	TaskID    string `json:"taskId,omitempty"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

type routineRunRecord struct {
	Definition       *routineDefinition `json:"definition,omitempty"`
	BindingsReleased bool               `json:"bindingsReleased,omitempty"`
	Graph            *routineFlow       `json:"graph,omitempty"`
	Steps            []routineStep      `json:"steps,omitempty"`
	TaskIDs          []string           `json:"taskIds,omitempty"`
	ID               string             `json:"runId"`
	RoutineID        string             `json:"routineId"`
	Revision         int                `json:"revision"`
	ScheduledAt      string             `json:"scheduledAt"`
	Source           string             `json:"source"`
	State            string             `json:"state"`
	Reason           string             `json:"reason,omitempty"`
	RequestKey       string             `json:"requestKey,omitempty"`
	SkippedCount     int                `json:"skippedCount,omitempty"`
	ConversationID   string             `json:"conversationId,omitempty"`
	Error            string             `json:"error,omitempty"`
	CreatedAt        string             `json:"createdAt"`
	UpdatedAt        string             `json:"updatedAt"`
}

type routineTokenView struct {
	RoutineIDs []string `json:"routineIds"`
	ID         string   `json:"id"`
	ExpiresAt  string   `json:"expiresAt,omitempty"`
	RevokedAt  string   `json:"revokedAt,omitempty"`
	CreatedAt  string   `json:"createdAt"`
	// Token is returned exactly once, by the create operation.
	Token string `json:"token,omitempty"`
}

// routineSchedulers tracks the single durable scanner per relay.
var routineSchedulers sync.Map

func routineTime(t time.Time) string { return t.UTC().Format(routineTimeLayout) }

func routineParseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	return time.Parse(time.RFC3339Nano, value)
}

func routineTruncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func routineContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func routineTerminal(state string) bool {
	return state == routineRunCompleted || state == routineRunFailed || state == routineRunCancelled || state == routineRunSkipped
}

// routineOperatorPrincipal is the trusted operator identity used to admit
// operator-authored routine work. It is never handed to a trigger source.
func routineOperatorPrincipal() model.Principal { return model.Principal{Admin: true} }

// Trigger validation keeps schedules explicit: five standard cron fields, a
// bounded interval, an explicit instant, or an explicit ingress mode.
func (t routineTrigger) validate() error {
	switch t.Kind {
	case "once":
		if t.At == "" {
			return model.Err(400, "once trigger requires at")
		}
		if _, err := routineTriggerInstant(t); err != nil {
			return err
		}
		if t.EverySeconds != 0 || t.Expression != "" {
			return model.Err(400, "once trigger accepts only at")
		}
	case "interval":
		if t.EverySeconds < routineMinInterval {
			return model.Err(400, "interval trigger requires everySeconds of at least 60")
		}
		if t.At != "" || t.Expression != "" {
			return model.Err(400, "interval trigger accepts only everySeconds")
		}
	case "cron":
		if t.Expression == "" {
			return model.Err(400, "cron trigger requires expression")
		}
		if err := routineValidateCron(t.Expression); err != nil {
			return err
		}
		if t.At != "" || t.EverySeconds != 0 {
			return model.Err(400, "cron trigger accepts only expression")
		}
	case "webhook", "manual":
		if t.At != "" || t.EverySeconds != 0 || t.Expression != "" {
			return model.Err(400, "trigger accepts no schedule fields")
		}
	default:
		return model.Err(400, "trigger kind must be once, interval, cron, webhook or manual")
	}
	if t.TimeZone != "" {
		if _, err := time.LoadLocation(t.TimeZone); err != nil {
			return model.Err(400, "unknown time zone")
		}
	}
	return nil
}

func routineValidateCron(expression string) error {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return model.Err(400, "cron expression is required")
	}
	if strings.HasPrefix(trimmed, "@") {
		return model.Err(400, "cron descriptors are not supported; use five standard fields")
	}
	if len(strings.Fields(trimmed)) != 5 {
		return model.Err(400, "cron expression must use five standard fields")
	}
	if _, err := cron.ParseStandard(trimmed); err != nil {
		return model.Err(400, "invalid cron expression: "+err.Error())
	}
	return nil
}

func routineLocation(d routineDefinition) *time.Location {
	if d.Trigger.TimeZone == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(d.Trigger.TimeZone)
	if err != nil {
		return time.UTC
	}
	return location
}

// routineTriggerInstant resolves a one-shot instant. An explicit offset wins;
// a bare local time is interpreted in the trigger time zone.
func routineTriggerInstant(t routineTrigger) (time.Time, error) {
	location := time.UTC
	if t.TimeZone != "" {
		loaded, err := time.LoadLocation(t.TimeZone)
		if err != nil {
			return time.Time{}, model.Err(400, "unknown time zone")
		}
		location = loaded
	}
	if at, err := time.Parse(time.RFC3339, t.At); err == nil {
		return at.UTC(), nil
	}
	at, err := time.ParseInLocation("2006-01-02T15:04:05", t.At, location)
	if err != nil {
		return time.Time{}, model.Err(400, "at must be an RFC3339 timestamp or a local time")
	}
	return at.UTC(), nil
}

// routineNextOccurrence returns the first occurrence strictly after `after`.
func routineNextOccurrence(d routineDefinition, after time.Time) (time.Time, error) {
	switch d.Trigger.Kind {
	case "once":
		at, err := routineTriggerInstant(d.Trigger)
		if err != nil {
			return time.Time{}, err
		}
		if !at.After(after) {
			return time.Time{}, errRoutineNoOccurrence
		}
		return at, nil
	case "interval":
		if d.Trigger.EverySeconds < routineMinInterval {
			return time.Time{}, errRoutineNoOccurrence
		}
		return after.Add(time.Duration(d.Trigger.EverySeconds) * time.Second), nil
	case "cron":
		schedule, err := cron.ParseStandard(strings.TrimSpace(d.Trigger.Expression))
		if err != nil {
			return time.Time{}, model.Err(400, "invalid cron expression")
		}
		return schedule.Next(after.In(routineLocation(d))).UTC(), nil
	}
	return time.Time{}, errRoutineNoOccurrence
}

func (r *Relay) routineDefinition(id string) (routineDefinition, string, error) {
	var d routineDefinition
	var body, nextDue string
	err := r.db.QueryRow("SELECT body,next_due_at FROM routines WHERE id=?", id).Scan(&body, &nextDue)
	if errors.Is(err, sql.ErrNoRows) {
		return d, "", model.Err(404, "routine not found")
	}
	if err != nil {
		return d, "", err
	}
	if err = json.Unmarshal([]byte(body), &d); err != nil {
		return d, "", err
	}
	return d, nextDue, nil
}

func routineViewOf(d routineDefinition, nextDue string, secretStatus string) routineView {
	view := routineView{ID: d.ID, Revision: d.Revision, Name: d.Name, ProfileID: d.ProfileID, Instruction: d.Instruction, Trigger: d.Trigger, FlowID: d.FlowID, FlowRevision: d.FlowRevision, Enabled: d.Enabled, AutoStart: d.AutoStart, TimeoutSeconds: d.TimeoutSeconds, NextDueAt: nextDue, WebhookSecretStatus: secretStatus}
	return view
}

// routineSecretStatus reports the vault state of a routine webhook key.
func (r *Relay) routineSecretStatus(ctx context.Context, ref string) string {
	if ref == "" {
		return "missing"
	}
	status, err := r.secretStatus(ctx, ref)
	if err != nil {
		return "unreadable"
	}
	return status
}

// routineRunResponse drops the internal graph snapshot from an API response.
// The snapshot stays in the durable receipt.
func routineRunResponse(record routineRunRecord) routineRunRecord {
	record.Graph = nil
	record.Definition = nil
	record.BindingsReleased = false
	return record
}

func (r *Relay) loadRoutineRun(id string) (routineRunRecord, error) {
	var record routineRunRecord
	var body string
	err := r.db.QueryRow("SELECT body FROM routine_runs WHERE id=?", id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return record, model.Err(404, "routine run not found")
	}
	if err != nil {
		return record, err
	}
	err = json.Unmarshal([]byte(body), &record)
	return record, err
}

// routineRunDefinition retains the reviewed instruction and timeout across
// edits. Older receipts may use the current definition only at the same revision.
func (r *Relay) routineRunDefinition(record routineRunRecord) (routineDefinition, error) {
	if record.Definition != nil {
		return *record.Definition, nil
	}
	d, _, err := r.routineDefinition(record.RoutineID)
	if err != nil {
		return d, err
	}
	if d.Revision != record.Revision {
		return d, model.Err(409, "routine run definition revision is unavailable")
	}
	return d, nil
}

// routineOperation dispatches every routine and flow operation. Routine
// execution tokens may only run their granted routines; every other operation
// requires an administrator.
func (r *Relay) routineOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tokenID := p.RoutineTokenID; tokenID != "" {
		if operation != "routines.run" {
			return nil, model.Err(403, "routine credentials may only run their granted routines")
		}
		return r.routineRun(ctx, p, raw, tokenID)
	}
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	switch operation {
	case "routines.list":
		return r.routineList(ctx, raw)
	case "routines.save":
		return r.routineSave(ctx, p, raw)
	case "routines.preview":
		return r.routinePreview(raw)
	case "routines.setEnabled":
		return r.routineSetEnabled(ctx, raw)
	case "routines.run":
		return r.routineRun(ctx, p, raw, "")
	case "routines.runs":
		return r.routineRuns(raw)
	case "routines.cancelRun":
		return r.routineCancelRun(raw)
	case "flows.get":
		return r.flowGet(raw)
	case "flows.save":
		return r.flowSave(raw)
	case "routineTokens.create":
		return r.routineTokenCreate(raw)
	case "routineTokens.list":
		return r.routineTokenList(raw)
	case "routineTokens.revoke":
		return r.routineTokenRevoke(raw)
	}
	return nil, model.Err(404, "unknown automation operation")
}

func (r *Relay) routineList(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		After string `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = routineDefaultPage
	}
	if q.Limit < 1 || q.Limit > routinePageLimit || len(q.After) > 64 {
		return nil, model.Err(400, "invalid routine page")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query("SELECT id,body,next_due_at FROM routines WHERE id>? ORDER BY id LIMIT ?", q.After, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := model.AutomationPage[routineView]{Items: []routineView{}}
	for rows.Next() {
		var id, body, nextDue string
		if err = rows.Scan(&id, &body, &nextDue); err != nil {
			return nil, err
		}
		var d routineDefinition
		if err = json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, routineViewOf(d, nextDue, r.routineSecretStatus(ctx, d.WebhookSecretRef)))
		if len(page.Items) > q.Limit {
			page.HasMore = true
			page.NextAfter = page.Items[q.Limit-1].ID
			page.Items = page.Items[:q.Limit]
			break
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

func (r *Relay) routineSave(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	var q struct {
		Routine          routineInput `json:"routine"`
		ExpectedRevision *int         `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	input := q.Routine
	r.mu.Lock()
	creating := input.ID == ""
	var existing routineDefinition
	var previousRef string
	if creating {
		if q.ExpectedRevision != nil {
			r.mu.Unlock()
			return nil, model.Err(400, "expectedRevision is not valid for a new routine")
		}
	} else {
		stored, _, err := r.routineDefinition(input.ID)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		if q.ExpectedRevision == nil || *q.ExpectedRevision != stored.Revision {
			r.mu.Unlock()
			return nil, model.Err(409, "routine revision changed")
		}
		existing = stored
		previousRef = stored.WebhookSecretRef
	}
	// A partial update keeps the stored schedule, profile, instruction and
	// webhook key; an explicit value replaces it.
	d := routineDefinition{ID: input.ID, Revision: 1, Name: input.Name, ProfileID: input.ProfileID, Instruction: input.Instruction, TimeoutSeconds: input.TimeoutSeconds, FlowID: "", FlowRevision: input.FlowRevision}
	if !creating {
		d = existing
		d.Revision = existing.Revision + 1
	}
	if input.Name != "" {
		d.Name = input.Name
	}
	if input.ProfileID != "" {
		d.ProfileID = input.ProfileID
	}
	if input.Instruction != "" {
		d.Instruction = input.Instruction
	}
	if input.Trigger != nil {
		d.Trigger = *input.Trigger
	}
	if input.Enabled != nil {
		d.Enabled = *input.Enabled
	}
	if input.AutoStart != nil {
		d.AutoStart = *input.AutoStart
	}
	if input.FlowID != nil {
		d.FlowID = *input.FlowID
		d.FlowRevision = input.FlowRevision
	}
	if input.TimeoutSeconds != 0 {
		d.TimeoutSeconds = input.TimeoutSeconds
	}
	if creating {
		d.ID = randomID()
	}
	if err := r.validateRoutineDefinition(&d, creating); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	// The webhook key is an encrypted vault record; the plaintext never enters
	// the routine record, an event or a log line.
	generated := ""
	secretRef := d.WebhookSecretRef
	switch {
	case input.WebhookSecret != "":
		if len(input.WebhookSecret) < 32 || len(input.WebhookSecret) > 256 || !safeRegistryText(input.WebhookSecret) {
			r.mu.Unlock()
			return nil, model.Err(400, "webhookSecret must be 32..256 bytes of safe text")
		}
		stored, err := r.storeSecret(ctx, "routine_webhook", input.WebhookSecret)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		secretRef = stored
	case d.Trigger.Kind == "webhook" && secretRef == "":
		secret, err := routineNewSecret()
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		stored, err := r.storeSecret(ctx, "routine_webhook", secret)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		secretRef = stored
		generated = secret
	}
	d.WebhookSecretRef = secretRef
	nextDue := ""
	if d.Enabled {
		switch d.Trigger.Kind {
		case "once", "interval", "cron":
			at, err := routineNextOccurrence(d, now())
			if err != nil {
				if !errors.Is(err, errRoutineNoOccurrence) {
					r.mu.Unlock()
					return nil, err
				}
			} else {
				nextDue = routineTime(at)
			}
		}
	}
	body, err := json.Marshal(d)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	defer tx.Rollback()
	if creating {
		if _, err = tx.Exec("INSERT INTO routines(id,revision,body,next_due_at,enabled) VALUES(?,?,?,?,?)", d.ID, d.Revision, string(body), nextDue, routineEnabledValue(d.Enabled)); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	} else {
		if _, err = tx.Exec("UPDATE routines SET revision=?,body=?,next_due_at=?,enabled=? WHERE id=? AND revision=?", d.Revision, string(body), nextDue, routineEnabledValue(d.Enabled), d.ID, d.Revision-1); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	if err = insertAutomationEvent(tx, "routines.saved", d.ID, d.ProfileID, map[string]any{"revision": d.Revision, "enabled": d.Enabled, "trigger": d.Trigger.Kind}); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()
	if previousRef != "" && previousRef != secretRef {
		// The replaced key is removed only after the new record is durable.
		_ = r.deleteSecretLocked(ctx, previousRef)
	}
	view := routineViewOf(d, nextDue, r.routineSecretStatus(ctx, secretRef))
	view.WebhookSecret = generated
	return view, nil
}

func (r *Relay) validateRoutineDefinition(d *routineDefinition, creating bool) error {
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > routineMaxNameBytes || !utf8.ValidString(d.Name) || !safeRegistryText(d.Name) || redact(d.Name) != d.Name {
		return model.Err(400, "routine name must be 1..128 bytes of safe text")
	}
	if strings.TrimSpace(d.Instruction) == "" || len(d.Instruction) > routineMaxInstruction || !utf8.ValidString(d.Instruction) {
		return model.Err(400, "routine instruction must be 1..65536 bytes of valid UTF-8")
	}
	if !profileIdentifier(d.ProfileID) {
		return model.Err(400, "routine profileId is invalid")
	}
	profile, err := r.profile(d.ProfileID)
	if err != nil {
		return err
	}
	if profile.Archived {
		return model.Err(409, "routine profile is archived")
	}
	if err = d.Trigger.validate(); err != nil {
		return err
	}
	if d.Trigger.Kind == "once" {
		at, err := routineTriggerInstant(d.Trigger)
		if err != nil {
			return err
		}
		if creating && !at.After(now()) {
			return model.Err(400, "once trigger requires a future timestamp")
		}
	}
	if d.TimeoutSeconds == 0 {
		d.TimeoutSeconds = routineDefaultTimeout
	}
	if d.TimeoutSeconds < 1 || d.TimeoutSeconds > routineMaxTimeout {
		return model.Err(400, "timeoutSeconds must be 1..86400")
	}
	if d.FlowID != "" {
		if !profileIdentifier(d.FlowID) {
			return model.Err(400, "flowId is invalid")
		}
		if _, err = r.loadRoutineFlow(d.FlowID, d.FlowRevision); err != nil {
			return err
		}
	}
	return nil
}

func routineEnabledValue(value bool) int {
	if value {
		return 1
	}
	return 0
}

// routineNewToken issues a 256-bit execution token exactly once.
func routineNewToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func routineNewSecret() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func (r *Relay) routineSetEnabled(ctx context.Context, raw json.RawMessage) (any, error) {
	var q struct {
		ID               string `json:"id"`
		Enabled          bool   `json:"enabled"`
		ExpectedRevision int    `json:"expectedRevision"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) {
		return nil, model.Err(400, "routine id is invalid")
	}
	if q.ExpectedRevision < 1 {
		return nil, model.Err(400, "expectedRevision is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, _, err := r.routineDefinition(q.ID)
	if err != nil {
		return nil, err
	}
	if d.Revision != q.ExpectedRevision {
		return nil, model.Err(409, "routine revision changed")
	}
	if q.Enabled && d.Trigger.Kind == "webhook" && d.WebhookSecretRef == "" {
		return nil, model.Err(409, "review and save the webhook draft to provision its secret before enabling")
	}
	d.Enabled = q.Enabled
	d.Revision = d.Revision + 1
	nextDue := ""
	if d.Enabled {
		switch d.Trigger.Kind {
		case "once", "interval", "cron":
			at, nextErr := routineNextOccurrence(d, now())
			if nextErr != nil {
				if !errors.Is(nextErr, errRoutineNoOccurrence) {
					return nil, nextErr
				}
			} else {
				nextDue = routineTime(at)
			}
		}
	}
	body, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE routines SET revision=?,body=?,next_due_at=?,enabled=? WHERE id=? AND revision=?", d.Revision, string(body), nextDue, routineEnabledValue(d.Enabled), d.ID, q.ExpectedRevision); err != nil {
		return nil, err
	}
	if err = insertAutomationEvent(tx, "routines.enabled", d.ID, d.ProfileID, map[string]any{"revision": d.Revision, "enabled": d.Enabled}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return routineViewOf(d, nextDue, r.routineSecretStatus(ctx, d.WebhookSecretRef)), nil
}

func (r *Relay) routinePreview(raw json.RawMessage) (any, error) {
	var q struct {
		Trigger routineTrigger `json:"trigger"`
		Count   int            `json:"count"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if err := q.Trigger.validate(); err != nil {
		return nil, err
	}
	if q.Count == 0 {
		q.Count = 5
	}
	if q.Count < 1 || q.Count > routineMaxPreview {
		return nil, model.Err(400, "count must be 1..10")
	}
	definition := routineDefinition{ID: "preview", Revision: 1, Name: "preview", ProfileID: "preview", Instruction: "preview", Trigger: q.Trigger, TimeoutSeconds: routineDefaultTimeout}
	cursor := now()
	occurrences := []string{}
	for range q.Count {
		at, err := routineNextOccurrence(definition, cursor)
		if err != nil {
			if errors.Is(err, errRoutineNoOccurrence) {
				break
			}
			return nil, err
		}
		occurrences = append(occurrences, at.UTC().Format(time.RFC3339))
		cursor = at
	}
	zone := q.Trigger.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	return map[string]any{"trigger": q.Trigger, "timeZone": zone, "occurrences": occurrences}, nil
}

// routineRun admits one occurrence of a routine. A console run explicitly
// authorizes starting the run's bindings; a token run stays subject to the
// reviewed autoStart grant.
func (r *Relay) routineRun(ctx context.Context, p model.Principal, raw json.RawMessage, tokenID string) (any, error) {
	var q struct {
		ID             string `json:"id"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ID) {
		return nil, model.Err(400, "routine id is invalid")
	}
	if len(q.IdempotencyKey) < 1 || len(q.IdempotencyKey) > 200 || !safeRegistryText(q.IdempotencyKey) || redact(q.IdempotencyKey) != q.IdempotencyKey {
		return nil, model.Err(400, "idempotencyKey must be a safe nonempty identifier")
	}
	r.mu.Lock()
	d, _, err := r.routineDefinition(q.ID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if !d.Enabled {
		r.mu.Unlock()
		return nil, model.Err(409, "routine is disabled")
	}
	if tokenID != "" {
		grants, grantErr := r.routineTokenGrants(tokenID)
		if grantErr != nil {
			r.mu.Unlock()
			return nil, grantErr
		}
		if !routineContains(grants, d.ID) {
			r.mu.Unlock()
			return nil, model.Err(403, "routine credential does not grant this routine")
		}
	}
	key := tokenHash("routine-run\x00" + d.ID + "\x00" + q.IdempotencyKey)
	existing, err := r.routineRunByRequestKey(key)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if existing != nil {
		r.mu.Unlock()
		return routineRunResponse(*existing), nil
	}
	source := routineSourceManual
	if tokenID != "" {
		source = routineSourceToken
	}
	record, err := r.createRoutineRun(d, source, now(), routineRunClaimed, "", key)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()
	operator := routineOperatorPrincipal()
	if tokenID == "" {
		operator = p
	}
	start := tokenID == "" || d.AutoStart
	if !start {
		r.mu.Lock()
		skipped, skipErr := r.markRoutineRunSkipped(record.ID, routineReasonAutoStart)
		r.mu.Unlock()
		if skipErr != nil {
			return nil, skipErr
		}
		return routineRunResponse(skipped), nil
	}
	run, err := r.admitRoutineRun(ctx, operator, record.ID, source, true)
	if err != nil {
		return nil, err
	}
	return routineRunResponse(run), nil
}

// routineRunByRequestKey resolves a previous admission of the same idempotency
// key. The key is stored hashed; the request itself is never persisted.
func (r *Relay) routineRunByRequestKey(key string) (*routineRunRecord, error) {
	var runID string
	err := r.db.QueryRow("SELECT id FROM routine_runs WHERE json_extract(body,'$.requestKey')=? LIMIT 1", key).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record, err := r.loadRoutineRun(runID)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// createRoutineRun inserts the durable occurrence receipt.
func (r *Relay) createRoutineRun(d routineDefinition, source string, scheduledAt time.Time, state string, reason string, requestKey string) (routineRunRecord, error) {
	record := routineRunRecord{ID: randomID(), RoutineID: d.ID, Revision: d.Revision, ScheduledAt: routineTime(scheduledAt), Source: source, State: state, Reason: reason, RequestKey: requestKey, Steps: []routineStep{}, TaskIDs: []string{}, CreatedAt: routineTime(now()), UpdatedAt: routineTime(now())}
	record.Definition = &d
	body, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,?,?,?,?,?,?)", record.ID, d.ID, d.Revision, record.ScheduledAt, source, state, string(body)); err != nil {
		return record, err
	}
	if err = insertAutomationEvent(tx, "routines."+state, record.ID, d.ProfileID, map[string]any{"routineId": d.ID, "source": source, "reason": reason, "scheduledAt": record.ScheduledAt}); err != nil {
		return record, err
	}
	if err = tx.Commit(); err != nil {
		return record, err
	}
	return record, nil
}

func (r *Relay) markRoutineRunSkipped(runID string, reason string) (routineRunRecord, error) {
	record, err := r.loadRoutineRun(runID)
	if err != nil {
		return record, err
	}
	record.State = routineRunSkipped
	record.Reason = reason
	record.UpdatedAt = routineTime(now())
	body, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE routine_runs SET state=?,body=? WHERE id=?", record.State, string(body), runID); err != nil {
		return record, err
	}
	if err = insertAutomationEvent(tx, "routines.skipped", runID, record.RoutineID, map[string]any{"reason": reason}); err != nil {
		return record, err
	}
	if err = tx.Commit(); err != nil {
		return record, err
	}
	return record, nil
}

func (r *Relay) routineRuns(raw json.RawMessage) (any, error) {
	var q struct {
		RoutineID string `json:"id"`
		After     string `json:"after"`
		Limit     int    `json:"limit"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = routineDefaultPage
	}
	if q.Limit < 1 || q.Limit > routinePageLimit || len(q.After) > 64 {
		return nil, model.Err(400, "invalid routine run page")
	}
	if !profileIdentifier(q.RoutineID) {
		return nil, model.Err(400, "routine id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	query := "SELECT id,body FROM routine_runs"
	where := []string{"routine_id=?"}
	args := []any{q.RoutineID}
	if q.After != "" {
		var rowID int64
		if err := r.db.QueryRow("SELECT rowid FROM routine_runs WHERE id=?", q.After).Scan(&rowID); err != nil {
			return nil, model.Err(400, "invalid routine run cursor")
		}
		where = append(where, "rowid<?")
		args = append(args, rowID)
	}
	query += " WHERE " + strings.Join(where, " AND ")
	query += " ORDER BY rowid DESC LIMIT ?"
	args = append(args, q.Limit+1)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := model.AutomationPage[routineRunRecord]{Items: []routineRunRecord{}}
	for rows.Next() {
		var id, body string
		if err = rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		var record routineRunRecord
		if err = json.Unmarshal([]byte(body), &record); err != nil {
			return nil, err
		}
		page.Items = append(page.Items, routineRunResponse(record))
		if len(page.Items) > q.Limit {
			page.HasMore = true
			page.NextAfter = page.Items[q.Limit-1].ID
			page.Items = page.Items[:q.Limit]
			break
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// routineCancelRun cancels the run's tasks and releases deferred reservations.
func (r *Relay) routineCancelRun(raw json.RawMessage) (any, error) {
	var q struct {
		RunID string `json:"runId"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if len(q.RunID) != 32 {
		return nil, model.Err(400, "runId is invalid")
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	record, err := r.cancelRoutineRunLocked(q.RunID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = r.releaseRoutineBindings(record.ID); err != nil {
		return nil, err
	}
	return routineRunResponse(record), nil
}

func (r *Relay) cancelRoutineRunLocked(runID string) (routineRunRecord, error) {
	record, err := r.loadRoutineRun(runID)
	if err != nil || routineTerminal(record.State) {
		return record, err
	}
	if err = r.cancelRoutineRunTasks(&record, "routine run cancelled by the operator; started execution may have external effects"); err != nil {
		return record, err
	}
	for i := range record.Steps {
		if record.Steps[i].State == routineStepReserved {
			record.Steps[i].State = routineStepCancelled
			record.Steps[i].Reason = "cancelled"
		}
	}
	record.State = routineRunCancelled
	record.Reason = "cancelled"
	record.UpdatedAt = routineTime(now())
	tx, err := r.db.Begin()
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	if err = storeRoutineRunRecordTx(tx, record); err != nil {
		return record, err
	}
	if err = insertAutomationEvent(tx, "routines.cancelled", record.ID, record.RoutineID, map[string]any{"reason": "cancelled"}); err != nil {
		return record, err
	}
	if err = tx.Commit(); err != nil {
		return record, err
	}
	return record, nil
}

// cancelRoutineRunTasks preserves tasks already settled by the scheduler and
// interrupts active native turns as well as invalidating pending tool decisions.
// The caller holds r.mu and persists the corresponding run/step changes afterward.
func (r *Relay) cancelRoutineRunTasks(record *routineRunRecord, reason string) error {
	if _, err := r.refreshRoutineStepStates(record); err != nil {
		return err
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != routineStepAdmitted || step.TaskID == "" {
			continue
		}
		task, err := r.task(step.TaskID)
		if err != nil {
			return err
		}
		task.State = "cancelled"
		task.ErrorCategory = "cancelled"
		task.Ambiguous = task.Attempts > 0
		task.Error = reason
		if err = r.transition(task, "cancelled", task.Error); err != nil {
			return err
		}
		if s := r.slots[task.To]; s != nil && s.taskID == task.ID && s.turnCancel != nil {
			s.turnCancel()
		}
		step.State = routineStepCancelled
		step.Reason = "cancelled"
	}
	return nil
}

func routineJSONBody(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(body)
}

// Routine execution tokens authenticate as a narrow, non-administrative
// principal: they may only run their granted routines.
func (r *Relay) routineTokenCreate(raw json.RawMessage) (any, error) {
	var q struct {
		RoutineIDs []string `json:"routineIds"`
		ExpiresAt  string   `json:"expiresAt"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if len(q.RoutineIDs) == 0 || len(q.RoutineIDs) > 64 {
		return nil, model.Err(400, "routineIds must contain 1..64 routines")
	}
	seen := map[string]bool{}
	for _, id := range q.RoutineIDs {
		if !profileIdentifier(id) || seen[id] {
			return nil, model.Err(400, "routineIds must be distinct valid identifiers")
		}
		seen[id] = true
	}
	expires := ""
	if q.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, q.ExpiresAt)
		if err != nil {
			return nil, model.Err(400, "expiresAt must be an RFC3339 timestamp")
		}
		if !at.After(now()) {
			return nil, model.Err(400, "expiresAt must be in the future")
		}
		expires = routineTime(at)
	}
	token, err := routineNewToken()
	if err != nil {
		return nil, err
	}
	id := randomID()
	view := routineTokenView{ID: id, RoutineIDs: append([]string{}, q.RoutineIDs...), ExpiresAt: expires, CreatedAt: routineTime(now()), Token: token}
	stored := view
	stored.Token = ""
	body, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, routineID := range q.RoutineIDs {
		if _, _, err = r.routineDefinition(routineID); err != nil {
			return nil, err
		}
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO routine_execution_tokens(id,token_hash,expires_at,revoked_at,body) VALUES(?,?,?,'',?)", id, tokenHash(token), expires, string(body)); err != nil {
		return nil, err
	}
	for _, routineID := range q.RoutineIDs {
		if _, err = tx.Exec("INSERT INTO routine_token_grants(token_id,routine_id) VALUES(?,?)", id, routineID); err != nil {
			return nil, err
		}
	}
	if err = insertAutomationEvent(tx, "routineTokens.created", id, "", map[string]any{"routineIds": q.RoutineIDs, "expiresAt": expires}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// The secret is never persisted in clear text; only its hash is stored.
	view.Token = token
	return view, nil
}

func (r *Relay) routineTokenList(raw json.RawMessage) (any, error) {
	var q struct {
		After string `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = routineDefaultPage
	}
	if q.Limit < 1 || q.Limit > routinePageLimit || len(q.After) > 64 {
		return nil, model.Err(400, "invalid routine token page")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.db.Query("SELECT id,body FROM routine_execution_tokens WHERE id>? ORDER BY id LIMIT ?", q.After, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := model.AutomationPage[routineTokenView]{Items: []routineTokenView{}}
	for rows.Next() {
		var id, body string
		if err = rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		var view routineTokenView
		if err = json.Unmarshal([]byte(body), &view); err != nil {
			return nil, err
		}
		view.Token = ""
		page.Items = append(page.Items, view)
		if len(page.Items) > q.Limit {
			page.HasMore = true
			page.NextAfter = page.Items[q.Limit-1].ID
			page.Items = page.Items[:q.Limit]
			break
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

func (r *Relay) routineTokenRevoke(raw json.RawMessage) (any, error) {
	var q struct {
		ID string `json:"id"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if len(q.ID) != 32 {
		return nil, model.Err(400, "routine token id is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var body, revoked string
	err := r.db.QueryRow("SELECT body,revoked_at FROM routine_execution_tokens WHERE id=?", q.ID).Scan(&body, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.Err(404, "routine token not found")
	}
	if err != nil {
		return nil, err
	}
	var view routineTokenView
	if err = json.Unmarshal([]byte(body), &view); err != nil {
		return nil, err
	}
	view.Token = ""
	if revoked == "" {
		revoked = routineTime(now())
		view.RevokedAt = revoked
		encoded, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE routine_execution_tokens SET revoked_at=?,body=? WHERE id=? AND revoked_at=''", revoked, string(encoded), q.ID); err != nil {
			return nil, err
		}
		if err = insertAutomationEvent(tx, "routineTokens.revoked", q.ID, "", nil); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	return view, nil
}

func (r *Relay) routineTokenGrants(tokenID string) ([]string, error) {
	rows, err := r.db.Query("SELECT routine_id FROM routine_token_grants WHERE token_id=?", tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		grants = append(grants, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return grants, nil
}

// AuthenticateRoutineToken resolves a routine execution token to its narrow
// principal. The token is never an administrator and never approves actions.
func (r *Relay) AuthenticateRoutineToken(token string) (model.Principal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.routineTokenPrincipal(token)
}

func (r *Relay) routineTokenPrincipal(token string) (model.Principal, error) {
	if strings.TrimSpace(token) == "" {
		return model.Principal{}, model.Err(401, "routine credential required")
	}
	var id, expires, revoked string
	err := r.db.QueryRow("SELECT id,expires_at,revoked_at FROM routine_execution_tokens WHERE token_hash=?", tokenHash(token)).Scan(&id, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Principal{}, model.Err(401, "invalid routine credential")
	}
	if err != nil {
		return model.Principal{}, err
	}
	if revoked != "" {
		return model.Principal{}, model.Err(401, "routine credential revoked")
	}
	if expires != "" {
		deadline, err := routineParseTime(expires)
		if err != nil {
			return model.Principal{}, model.Err(401, "routine credential expiry is invalid")
		}
		if now().After(deadline) {
			return model.Principal{}, model.Err(401, "routine credential expired")
		}
	}
	return model.Principal{RoutineTokenID: id}, nil
}

// RoutineWebhook verifies an ingress signature and claims the occurrence. The
// durable receipt is written before any task is admitted.
func (r *Relay) RoutineWebhook(ctx context.Context, routineID, timestamp, eventID, signature string, body []byte) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) > routineWebhookMaxBody {
		return nil, model.Err(413, "webhook body exceeds 65536 bytes")
	}
	if err := routineValidateEventID(eventID); err != nil {
		return nil, err
	}
	stamp, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return nil, model.Err(401, "invalid webhook timestamp")
	}
	if delta := now().Sub(time.Unix(stamp, 0)); delta < -routineWebhookWindow || delta > routineWebhookWindow {
		return nil, model.Err(401, "webhook timestamp is outside the accepted window")
	}
	r.mu.Lock()
	d, _, err := r.routineDefinition(routineID)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if d.Trigger.Kind != "webhook" {
		r.mu.Unlock()
		return nil, model.Err(409, "routine does not accept webhook events")
	}
	if !d.Enabled {
		r.mu.Unlock()
		return nil, model.Err(409, "routine is disabled")
	}
	if d.WebhookSecretRef == "" {
		r.mu.Unlock()
		return nil, model.Err(503, "routine webhook secret is not configured")
	}
	secret, err := r.readSecret(ctx, d.WebhookSecretRef)
	if err != nil {
		r.mu.Unlock()
		return nil, model.Err(503, "routine webhook secret is unavailable")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + eventID + "\n"))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	provided := strings.TrimSpace(signature)
	if !strings.HasPrefix(provided, "sha256=") {
		r.mu.Unlock()
		return nil, model.Err(401, "invalid webhook signature")
	}
	provided = strings.TrimPrefix(provided, "sha256=")
	if len(provided) != 64 || subtle.ConstantTimeCompare([]byte(strings.ToLower(provided)), []byte(expected)) != 1 {
		r.mu.Unlock()
		return nil, model.Err(401, "invalid webhook signature")
	}
	requestHash := tokenHash(string(body))
	var existingRun, existingHash string
	err = r.db.QueryRow("SELECT run_id,request_hash FROM routine_trigger_receipts WHERE routine_id=? AND source=? AND event_id=?", d.ID, routineSourceWebhook, eventID).Scan(&existingRun, &existingHash)
	if err == nil {
		r.mu.Unlock()
		if existingHash != requestHash {
			return nil, model.Err(409, "event id was already used for a different payload")
		}
		record, loadErr := r.loadRoutineRun(existingRun)
		if loadErr != nil {
			return nil, loadErr
		}
		return routineRunResponse(record), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		r.mu.Unlock()
		return nil, err
	}
	record := routineRunRecord{ID: randomID(), RoutineID: d.ID, Revision: d.Revision, ScheduledAt: routineTime(now()), Source: routineSourceWebhook, State: routineRunClaimed, Steps: []routineStep{}, TaskIDs: []string{}, CreatedAt: routineTime(now()), UpdatedAt: routineTime(now())}
	record.Definition = &d
	encoded, err := json.Marshal(record)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO routine_trigger_receipts(routine_id,source,event_id,request_hash,run_id) VALUES(?,?,?,?,?)", d.ID, routineSourceWebhook, eventID, requestHash, record.ID); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,?,?,?,?,?,?)", record.ID, d.ID, d.Revision, record.ScheduledAt, routineSourceWebhook, routineRunClaimed, string(encoded)); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err = insertAutomationEvent(tx, "routines.webhook", record.ID, d.ProfileID, map[string]any{"eventId": eventID, "scheduledAt": record.ScheduledAt}); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()
	if !d.AutoStart {
		r.mu.Lock()
		skipped, skipErr := r.markRoutineRunSkipped(record.ID, routineReasonAutoStart)
		r.mu.Unlock()
		if skipErr != nil {
			return nil, skipErr
		}
		return routineRunResponse(skipped), nil
	}
	run, err := r.admitRoutineRun(ctx, routineOperatorPrincipal(), record.ID, routineSourceWebhook, true)
	if err != nil {
		return nil, err
	}
	return routineRunResponse(run), nil
}

func routineValidateEventID(eventID string) error {
	if eventID == "" || len(eventID) > routineMaxEventIDBytes {
		return model.Err(400, "event id must be 1..200 bytes")
	}
	for _, ch := range eventID {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-' || ch == '_' || ch == '.' || ch == ':':
		default:
			return model.Err(400, "event id must contain only safe identifier bytes")
		}
	}
	return nil
}

// ensureRoutineScheduler starts the durable scanner once per relay. The
// scanner is the authority for due occurrences; in-memory timers never admit
// work on their own.
func (r *Relay) ensureRoutineScheduler() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if _, loaded := routineSchedulers.LoadOrStore(r, struct{}{}); loaded {
		return
	}
	r.wg.Add(1)
	go r.routineScheduler(r.ctx)
}

func (r *Relay) routineScheduler(ctx context.Context) {
	defer r.wg.Done()
	// Missed calendar occurrences are advanced without replay before the
	// scanner starts claiming live occurrences.
	r.routineRecoverDowntime()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.routineTick()
	}
}

// routineTick claims due occurrences and reconciles unsettled runs. It never
// holds the relay lock while a native runtime is started.
func (r *Relay) routineTick() {
	if !r.lifecycle.TryLock() {
		return
	}
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	claimed, err := r.claimDueRoutines()
	r.mu.Unlock()
	if err != nil {
		r.logger.Error("routine scan failed", "errorClass", "storage")
		return
	}
	for _, runID := range claimed {
		// A claimed scheduled occurrence already passed the reviewed autoStart
		// gate, so its reserved bindings may be started.
		if _, err = r.admitRoutineRunLocked(context.Background(), routineOperatorPrincipal(), runID, routineSourceSchedule, true); err != nil {
			r.logger.Warn("routine admission failed", "runId", runID)
		}
	}
	if err = r.reconcileRoutineRuns(); err != nil {
		r.logger.Error("routine reconciliation failed", "errorClass", "storage")
	}
}

// claimDueRoutines records exactly one receipt per due occurrence. The stored
// next_due_at, the revision and the enabled flag are rechecked under relay
// ownership immediately before the claim.
func (r *Relay) claimDueRoutines() ([]string, error) {
	rows, err := r.db.Query("SELECT id FROM routines WHERE enabled=1 AND next_due_at<>'' AND next_due_at<=? ORDER BY next_due_at LIMIT ?", routineTime(now()), routineScanLimit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	claimed := []string{}
	for _, id := range ids {
		runID, claimErr := r.claimRoutineOccurrence(id, routineSourceSchedule, true)
		if claimErr != nil {
			return claimed, claimErr
		}
		if runID != "" {
			claimed = append(claimed, runID)
		}
	}
	return claimed, nil
}

// routineAdvanceSchedule moves the stored next occurrence forward inside an
// open transaction.
func routineAdvanceSchedule(tx *sql.Tx, routineID string, next time.Time) error {
	if next.IsZero() {
		_, err := tx.Exec("UPDATE routines SET next_due_at='',enabled=0 WHERE id=?", routineID)
		return err
	}
	_, err := tx.Exec("UPDATE routines SET next_due_at=? WHERE id=?", routineTime(next), routineID)
	return err
}

// routineDuplicateOccurrence reports a unique violation on the occurrence key.
func routineDuplicateOccurrence(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// routineOccurrences reports the occurrences that are due at `at`, how many
// were missed and the next future occurrence. Occurrence keys are UTC
// instants, so a repeated wall clock time never duplicates an occurrence.
func routineOccurrences(d routineDefinition, due time.Time, at time.Time) (time.Time, time.Time, int, time.Time, error) {
	switch d.Trigger.Kind {
	case "once":
		if due.After(at) {
			return time.Time{}, time.Time{}, 0, due, nil
		}
		return due, due, 1, time.Time{}, nil
	case "interval":
		step := time.Duration(d.Trigger.EverySeconds) * time.Second
		if step <= 0 {
			return time.Time{}, time.Time{}, 0, time.Time{}, errRoutineNoOccurrence
		}
		if due.After(at) {
			return time.Time{}, time.Time{}, 0, due, nil
		}
		count := int(at.Sub(due)/step) + 1
		last := due.Add(time.Duration(count-1) * step)
		return due, last, count, last.Add(step), nil
	case "cron":
		schedule, err := cron.ParseStandard(strings.TrimSpace(d.Trigger.Expression))
		if err != nil {
			return time.Time{}, time.Time{}, 0, time.Time{}, model.Err(400, "invalid cron expression")
		}
		location := routineLocation(d)
		count := 0
		var first, last time.Time
		cursor := due
		for i := 0; i < 100000; i++ {
			if cursor.After(at) {
				return first, last, count, cursor, nil
			}
			if count == 0 {
				first = cursor
			}
			last = cursor
			count++
			cursor = schedule.Next(cursor.In(location)).UTC()
		}
		return first, last, count, schedule.Next(at.In(location)).UTC(), nil
	}
	return time.Time{}, time.Time{}, 0, time.Time{}, errRoutineNoOccurrence
}

// skipRoutineOccurrences records the skipped occurrences with a visible
// reason and advances the schedule to the next future occurrence.
func (r *Relay) skipRoutineOccurrences(d routineDefinition, at time.Time, count int, reason string, advanceTo time.Time) error {
	if count <= 0 {
		return nil
	}
	record := routineRunRecord{ID: randomID(), RoutineID: d.ID, Revision: d.Revision, ScheduledAt: routineTime(at), Source: routineSourceSchedule, State: routineRunSkipped, Reason: reason, SkippedCount: count, Steps: []routineStep{}, TaskIDs: []string{}, CreatedAt: routineTime(now()), UpdatedAt: routineTime(now())}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,?,?,?,?,?,?)", record.ID, d.ID, d.Revision, record.ScheduledAt, routineSourceSchedule, routineRunSkipped, string(body)); err != nil {
		if !routineDuplicateOccurrence(err) {
			return err
		}
		// The occurrence was already recorded; the schedule still advances.
		if err = routineAdvanceSchedule(tx, d.ID, advanceTo); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	if err = routineAdvanceSchedule(tx, d.ID, advanceTo); err != nil {
		return err
	}
	if err = insertAutomationEvent(tx, "routines.skipped", record.ID, d.ProfileID, map[string]any{"routineId": d.ID, "reason": reason, "skippedCount": count, "scheduledAt": record.ScheduledAt}); err != nil {
		return err
	}
	return tx.Commit()
}

// routineRecoverDowntime advances missed calendar occurrences without replay.
// It runs once when the durable scanner starts.
func (r *Relay) routineRecoverDowntime() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	rows, err := r.db.Query("SELECT id FROM routines WHERE enabled=1 AND next_due_at<>'' AND next_due_at<=? ORDER BY next_due_at LIMIT ?", routineTime(now()), routineScanLimit)
	if err != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		d, nextDue, err := r.routineDefinition(id)
		if err != nil {
			continue
		}
		due, err := routineParseTime(nextDue)
		if err != nil {
			continue
		}
		first, _, count, next, err := routineOccurrences(d, due, now())
		if err != nil || count == 0 {
			continue
		}
		if err = r.skipRoutineOccurrences(d, first, count, routineReasonDowntime, next); err != nil {
			return
		}
	}
}

// claimRoutineOccurrence claims one occurrence of a routine. Older missed
// occurrences are skipped, never replayed, and the schedule advances to the
// next future occurrence.
func (r *Relay) claimRoutineOccurrence(routineID string, source string, replay bool) (string, error) {
	d, nextDue, err := r.routineDefinition(routineID)
	if err != nil {
		return "", err
	}
	if !d.Enabled || nextDue == "" {
		return "", nil
	}
	due, err := routineParseTime(nextDue)
	if err != nil {
		return "", err
	}
	first, last, count, next, err := routineOccurrences(d, due, now())
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "", nil
	}
	if !replay {
		// Downtime never replays a missed occurrence.
		if err = r.skipRoutineOccurrences(d, first, count, routineReasonDowntime, next); err != nil {
			return "", err
		}
		return "", nil
	}
	if count > 1 {
		if err = r.skipRoutineOccurrences(d, first, count-1, routineReasonBacklog, last); err != nil {
			return "", err
		}
	}
	active, err := r.routineActiveRunCount(d.ID)
	if err != nil {
		return "", err
	}
	state := routineRunClaimed
	reason := ""
	if active > 0 {
		state = routineRunSkipped
		reason = routineReasonOverlap
	} else if source != routineSourceManual && !d.AutoStart {
		state = routineRunSkipped
		reason = routineReasonAutoStart
	}
	record := routineRunRecord{ID: randomID(), RoutineID: d.ID, Revision: d.Revision, ScheduledAt: routineTime(last), Source: source, State: state, Reason: reason, Steps: []routineStep{}, TaskIDs: []string{}, CreatedAt: routineTime(now()), UpdatedAt: routineTime(now())}
	record.Definition = &d
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO routine_runs(id,routine_id,revision,scheduled_at,source,state,body) VALUES(?,?,?,?,?,?,?)", record.ID, d.ID, d.Revision, record.ScheduledAt, source, state, string(body)); err != nil {
		if !routineDuplicateOccurrence(err) {
			return "", err
		}
		// The occurrence key is unique: a second claim is refused and the
		// schedule still advances to the next future occurrence.
		if err = routineAdvanceSchedule(tx, d.ID, next); err != nil {
			return "", err
		}
		if err = tx.Commit(); err != nil {
			return "", err
		}
		return "", nil
	}
	if err = routineAdvanceSchedule(tx, d.ID, next); err != nil {
		return "", err
	}
	if err = insertAutomationEvent(tx, "routines."+state, record.ID, d.ProfileID, map[string]any{"routineId": d.ID, "source": source, "reason": reason, "scheduledAt": record.ScheduledAt}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if state != routineRunClaimed {
		return "", nil
	}
	return record.ID, nil
}

func (r *Relay) routineActiveRunCount(routineID string) (int, error) {
	var count int
	err := r.db.QueryRow("SELECT count(*) FROM routine_runs WHERE routine_id=? AND state IN (?,?)", routineID, routineRunClaimed, routineRunRunning).Scan(&count)
	return count, err
}

func (r *Relay) failRoutineRun(runID string, reason string) (routineRunRecord, error) {
	record, err := r.loadRoutineRun(runID)
	if err != nil {
		return record, err
	}
	if routineTerminal(record.State) {
		return record, nil
	}
	if err = r.cancelRoutineRunTasks(&record, "routine admission failed; started execution may have external effects"); err != nil {
		return record, err
	}
	for i := range record.Steps {
		if record.Steps[i].State == routineStepReserved {
			record.Steps[i].State = routineStepSkipped
			record.Steps[i].Reason = "admission_failed"
		}
	}
	record.State = routineRunFailed
	record.Error = routineTruncate(redact(reason), 2048)
	record.UpdatedAt = routineTime(now())
	tx, err := r.db.Begin()
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	if err = storeRoutineRunRecordTx(tx, record); err != nil {
		return record, err
	}
	if err = insertAutomationEvent(tx, "routines.failed", runID, record.RoutineID, map[string]any{"reason": record.Error}); err != nil {
		return record, err
	}
	if err = tx.Commit(); err != nil {
		return record, err
	}
	return record, nil
}
