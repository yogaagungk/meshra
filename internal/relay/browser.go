package relay

// Isolated browser and desktop control. The browser runs inside the confined
// worker with its own managed profile, no host debugging port and no route to
// the host network except the mounted daemon egress socket. Human takeover and
// model input are separated by an epoch and a session owned lease.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"meshra/internal/model"
	"meshra/internal/netpolicy"
	"meshra/internal/sandbox"
)

const (
	browserMaxTabs          = 4
	browserSnapshotText     = 64 << 10
	browserModelImageLimit  = 512 << 10
	browserFrameLimit       = 2 << 20
	browserLeaseDuration    = 60 * time.Second
	browserIdleClose        = 30 * time.Minute
	browserActionTimeout    = 60 * time.Second
	browserMaxSelector      = 1024
	browserMaxText          = 4096
	browserMaxURL           = 2048
	browserMaxKey           = 64
	browserMaxScrollAmount  = 2000
	browserMaxStateFileSize = 4 << 20
)

const (
	browserStateOpening    = "opening"
	browserStateActive     = "active"
	browserStateTakingOver = "taking_over"
	browserStateClosed     = "closed"
	browserStateError      = "error"
)

// browserSession is the persisted control record of one isolated browser.
type browserSession struct {
	ID               string     `json:"id"`
	ProfileID        string     `json:"profileId"`
	ConversationID   string     `json:"conversationId"`
	TaskID           string     `json:"taskId,omitempty"`
	Mode             string     `json:"mode"`
	DataID           string     `json:"dataId"`
	ArtifactsExpired bool       `json:"artifactsExpired,omitempty"`
	State            string     `json:"state"`
	Epoch            int        `json:"epoch"`
	FrameSeq         int        `json:"frameSeq"`
	LeaseOwner       string     `json:"leaseOwner,omitempty"`
	LeaseExpiresAt   *time.Time `json:"leaseExpiresAt,omitempty"`
	SnapshotText     string     `json:"snapshotText,omitempty"`
	SnapshotAt       *time.Time `json:"snapshotAt,omitempty"`
	Tabs             int        `json:"tabs"`
	LastError        string     `json:"lastError,omitempty"`
	Invocations      []string   `json:"invocations,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

// browserAction is the strict action union shared by the console and the
// broker. Unknown fields are rejected before anything is dispatched.
type browserAction struct {
	Action       string `json:"action"`
	URL          string `json:"url,omitempty"`
	Selector     string `json:"selector,omitempty"`
	Text         string `json:"text,omitempty"`
	Key          string `json:"key,omitempty"`
	Direction    string `json:"direction,omitempty"`
	Amount       int    `json:"amount,omitempty"`
	Tab          int    `json:"tab,omitempty"`
	AttachmentID string `json:"attachmentId,omitempty"`
	Executable   string `json:"executable,omitempty"`
	X            int    `json:"x,omitempty"`
	Y            int    `json:"y,omitempty"`
	WindowID     int    `json:"windowId,omitempty"`
}

type browserResult struct {
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	Text       string `json:"text,omitempty"`
	Frame      string `json:"frame,omitempty"`
	FrameBytes int    `json:"frameBytes,omitempty"`
	Download   string `json:"download,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Tabs       int    `json:"tabs,omitempty"`
}

// browserWorkerAction is the internal wire shape of one action. The staged
// file path is never accepted from a caller: only the daemon places a file in
// the private browser state and names it here.
type browserWorkerAction struct {
	Action     string `json:"action"`
	URL        string `json:"url,omitempty"`
	Selector   string `json:"selector,omitempty"`
	Text       string `json:"text,omitempty"`
	Key        string `json:"key,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Amount     int    `json:"amount,omitempty"`
	Tab        int    `json:"tab,omitempty"`
	Path       string `json:"path,omitempty"`
	Executable string `json:"executable,omitempty"`
	X          int    `json:"x,omitempty"`
	Y          int    `json:"y,omitempty"`
	WindowID   int    `json:"windowId,omitempty"`
}

// browserWorker is the confined browser process behind one session.
type browserWorker interface {
	Execute(ctx context.Context, action browserWorkerAction) (browserResult, error)
	Frame(ctx context.Context) ([]byte, error)
	Close() error
}

type browserRuntime struct {
	mu         sync.Mutex
	control    sync.Mutex
	record     browserSession
	worker     browserWorker
	dispatch   sync.Mutex
	active     context.CancelFunc
	pending    map[string]bool
	lastFrame  time.Time
	frame      []byte
	dispatched map[string]bool
}

type browserManager struct {
	mu       sync.Mutex
	create   sync.Mutex
	closed   bool
	sessions map[string]*browserRuntime
}

func (r *Relay) browserManager() *browserManager {
	r.browserOnce.Do(func() {
		r.browserRegistry = &browserManager{sessions: map[string]*browserRuntime{}, closed: r.ctx.Err() != nil}
	})
	return r.browserRegistry
}

func (m *browserManager) get(id string) *browserRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *browserManager) put(runtime *browserRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[runtime.record.ID] = runtime
}

func (m *browserManager) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

func (m *browserManager) list() []*browserRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*browserRuntime, 0, len(m.sessions))
	for _, runtime := range m.sessions {
		out = append(out, runtime)
	}
	return out
}

func (runtime *browserRuntime) snapshot() browserSession {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.record
}

func (runtime *browserRuntime) store(record browserSession) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.record = record
}

func (runtime *browserRuntime) cancelActive() {
	runtime.mu.Lock()
	cancel := runtime.active
	runtime.active = nil
	for id := range runtime.pending {
		delete(runtime.pending, id)
	}
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---- validation ----------------------------------------------------------

// browserValidateAction rejects unknown actions, unknown fields and unsafe
// bounds before a worker is asked to do anything.
func browserValidateAction(action browserAction) error {
	switch action.Action {
	case "open", "navigate":
		if action.URL == "" {
			return model.Err(400, "a URL is required")
		}
		if len(action.URL) > browserMaxURL {
			return model.Err(413, "URL exceeds 2048 bytes")
		}
		parsed, err := url.Parse(action.URL)
		if err != nil {
			return model.Err(400, "invalid URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return model.Err(400, "only http and https URLs are allowed")
		}
		if parsed.User != nil {
			return model.Err(400, "URL credentials are not allowed")
		}
		if parsed.Host == "" {
			return model.Err(400, "URL host is required")
		}
	case "inspect", "screenshot", "close":
	case "click", "upload":
		if action.Selector == "" {
			return model.Err(400, "a selector is required")
		}
		if len(action.Selector) > browserMaxSelector || strings.ContainsRune(action.Selector, 0) {
			return model.Err(400, "invalid selector")
		}
		if action.Action == "upload" && action.AttachmentID == "" {
			return model.Err(400, "an attachmentId is required for upload")
		}
	case "type":
		if action.Selector == "" {
			return model.Err(400, "a selector is required")
		}
		if len(action.Selector) > browserMaxSelector || strings.ContainsRune(action.Selector, 0) {
			return model.Err(400, "invalid selector")
		}
		if action.Text == "" {
			return model.Err(400, "text is required")
		}
		if len(action.Text) > browserMaxText || strings.ContainsRune(action.Text, 0) {
			return model.Err(400, "invalid text")
		}
	case "key":
		if action.Key == "" {
			return model.Err(400, "a key is required")
		}
		if len(action.Key) > browserMaxKey || strings.ContainsRune(action.Key, 0) {
			return model.Err(400, "invalid key")
		}
	case "scroll":
		switch action.Direction {
		case "up", "down", "left", "right":
		case "":
			return model.Err(400, "a scroll direction is required")
		default:
			return model.Err(400, "invalid scroll direction")
		}
		if action.Amount == 0 {
			action.Amount = 400
		}
		if action.Amount < 1 || action.Amount > browserMaxScrollAmount {
			return model.Err(400, "invalid scroll amount")
		}
	case "download":
		if action.Selector != "" && len(action.Selector) > browserMaxSelector {
			return model.Err(400, "invalid selector")
		}
	default:
		return model.Err(400, "unknown browser action")
	}
	if action.Tab < 0 || action.Tab >= browserMaxTabs {
		return model.Err(400, "invalid tab index")
	}
	return nil
}

func browserValidateModeAction(mode string, action browserAction) error {
	if mode != "desktop" {
		if err := browserValidateAction(action); err != nil {
			return err
		}
		allowed := map[string]string{
			"open": "url,tab", "navigate": "url,tab", "inspect": "tab", "screenshot": "tab", "close": "tab",
			"click": "selector,tab", "type": "selector,text,tab", "key": "key,tab",
			"scroll": "direction,amount,tab", "upload": "selector,attachmentId,tab", "download": "selector,tab",
		}
		return browserRejectActionFields(action, allowed[action.Action])
	}
	if action.WindowID < 0 || action.WindowID > 1<<31-1 {
		return model.Err(400, "invalid private window id")
	}
	var fields string
	switch action.Action {
	case "launch":
		if action.Executable != "chromium" && action.Executable != "xterm" {
			return model.Err(400, "executable must be an operator-reviewed chromium or xterm allowlist entry")
		}
		fields = "executable"
	case "snapshot":
	case "click":
		if action.X < 0 || action.X >= 1280 || action.Y < 0 || action.Y >= 800 {
			return model.Err(400, "private desktop coordinates are outside 1280x800")
		}
		fields = "x,y,windowId"
	case "type":
		if action.Text == "" || len(action.Text) > browserMaxText || strings.ContainsRune(action.Text, 0) {
			return model.Err(400, "invalid desktop text")
		}
		fields = "text,windowId"
	case "key":
		if action.Key == "" || len(action.Key) > browserMaxKey || strings.ContainsAny(action.Key, "\x00\r\n") || strings.HasPrefix(action.Key, "-") {
			return model.Err(400, "invalid desktop key")
		}
		fields = "key,windowId"
	case "scroll":
		if action.Direction != "up" && action.Direction != "down" && action.Direction != "left" && action.Direction != "right" {
			return model.Err(400, "invalid scroll direction")
		}
		if action.Amount < 0 || action.Amount > browserMaxScrollAmount {
			return model.Err(400, "invalid scroll amount")
		}
		fields = "direction,amount,windowId"
	case "close":
		fields = "windowId"
	default:
		return model.Err(400, "unknown computer action")
	}
	return browserRejectActionFields(action, fields)
}

func browserRejectActionFields(action browserAction, fields string) error {
	allowed := map[string]bool{"action": true}
	for _, field := range strings.Split(fields, ",") {
		allowed[field] = true
	}
	body, _ := json.Marshal(action)
	var values map[string]any
	_ = json.Unmarshal(body, &values)
	for field := range values {
		if !allowed[field] {
			return model.Err(400, "field "+field+" is not valid for "+action.Action)
		}
	}
	return nil
}

func browserDataID(profileID, conversationID, mode string) string {
	hash := sha256.Sum256([]byte(profileID + "\x00" + conversationID + "\x00" + mode))
	return hex.EncodeToString(hash[:])
}

// browserValidateURL rejects destinations the operator never reviewed. The
// egress proxy performs the authoritative check; this keeps obviously wrong
// input out of the worker.
func browserValidateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return model.Err(400, "invalid URL")
	}
	host := parsed.Hostname()
	if host == "" {
		return model.Err(400, "URL host is required")
	}
	if strings.EqualFold(host, "localhost") {
		return model.Err(403, "a local development origin needs an explicit network grant")
	}
	return nil
}

// ---- persistence ---------------------------------------------------------

func (r *Relay) storeBrowserSession(session browserSession) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO browser_sessions(id,profile_id,conversation_id,task_id,state,epoch,body) VALUES(?,?,?,nullif(?,''),?,?,?) ON CONFLICT(id) DO UPDATE SET task_id=excluded.task_id,state=excluded.state,epoch=excluded.epoch,body=excluded.body", session.ID, session.ProfileID, session.ConversationID, session.TaskID, session.State, session.Epoch, string(body)); err != nil {
		return err
	}
	if err = insertAutomationEvent(tx, "browser."+session.State, session.ID, session.ProfileID, map[string]any{"conversationId": session.ConversationID, "epoch": session.Epoch}); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Relay) loadBrowserSession(id string) (browserSession, error) {
	var session browserSession
	var body string
	var state string
	var epoch int
	err := r.db.QueryRow("SELECT body,state,epoch FROM browser_sessions WHERE id=?", id).Scan(&body, &state, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return session, model.Err(404, "browser session not found")
	}
	if err != nil {
		return session, err
	}
	if err = json.Unmarshal([]byte(body), &session); err != nil {
		return session, err
	}
	session.State = state
	session.Epoch = epoch
	return session, nil
}

// browserInvalidateInvocations marks pending approvals of one browser session
// stale. A takeover or release never lets an approved old epoch action run.
func browserInvalidateInvocations(tx *sql.Tx, invocations []string, reason string) error {
	if len(invocations) == 0 {
		return nil
	}
	for _, id := range invocations {
		if id == "" {
			continue
		}
		if _, err := tx.Exec("UPDATE tool_approvals SET status='invalidated',revision=revision+1 WHERE invocation_id=? AND status IN ('pending','approved')", id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE id=? AND state IN ('pending','approved','waiting_question')", id); err != nil {
			return err
		}
	}
	return nil
}

// ---- operations ----------------------------------------------------------

type browserOpenRequest struct {
	ProfileID      string `json:"profileId"`
	ConversationID string `json:"conversationId"`
	TaskID         string `json:"taskId"`
	Mode           string `json:"mode,omitempty"`
}

type browserSessionRequest struct {
	ID string `json:"id"`
}

type browserEpochRequest struct {
	ID            string `json:"id"`
	ExpectedEpoch int    `json:"expectedEpoch"`
}

type browserInputRequest struct {
	ID       string        `json:"id"`
	Epoch    int           `json:"epoch"`
	FrameSeq int           `json:"frameSeq"`
	Action   browserAction `json:"action"`
}

type browserClearRequest struct {
	ProfileID      string `json:"profileId"`
	ConversationID string `json:"conversationId"`
	Confirm        bool   `json:"confirm"`
}

// BrowserToolArgs is the broker argument shape for meshra_browser and
// meshra_computer. The integration owner decodes it with decodeAutomation.
type BrowserToolArgs struct {
	SessionID    string `json:"sessionId,omitempty"`
	Action       string `json:"action"`
	URL          string `json:"url,omitempty"`
	Selector     string `json:"selector,omitempty"`
	Text         string `json:"text,omitempty"`
	Key          string `json:"key,omitempty"`
	Direction    string `json:"direction,omitempty"`
	Amount       int    `json:"amount,omitempty"`
	Tab          int    `json:"tab,omitempty"`
	AttachmentID string `json:"attachmentId,omitempty"`
	Executable   string `json:"executable,omitempty"`
	Epoch        int    `json:"epoch"`
	FrameSeq     int    `json:"frameSeq"`
	X            int    `json:"x,omitempty"`
	Y            int    `json:"y,omitempty"`
	WindowID     int    `json:"windowId,omitempty"`
}

func (r *Relay) browserOperation(ctx context.Context, p model.Principal, operation string, raw json.RawMessage) (value any, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = requireAdmin(p); err != nil {
		return nil, err
	}
	defer func() {
		if session, ok := value.(browserSession); ok {
			session.LeaseOwner = ""
			session.Invocations = nil
			session.DataID = ""
			value = session
		}
	}()
	switch operation {
	case "browser.open":
		return r.browserOpen(ctx, p, raw)
	case "browser.state":
		return r.browserState(p, raw)
	case "browser.sessions":
		return r.browserSessions(p, raw)
	case "browser.takeover":
		return r.browserTakeover(ctx, p, raw)
	case "browser.release":
		return r.browserRelease(ctx, p, raw)
	case "browser.input":
		return r.browserInput(ctx, p, raw)
	case "browser.close":
		return r.browserClose(ctx, p, raw)
	case "browser.clearData":
		return r.browserClearData(ctx, p, raw)
	}
	return nil, model.Err(404, "unknown browser operation")
}

func (r *Relay) browserSessions(p model.Principal, raw json.RawMessage) (any, error) {
	var q struct {
		ProfileID      string `json:"profileId"`
		ConversationID string `json:"conversationId"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !profileIdentifier(q.ProfileID) || q.ConversationID == "" {
		return nil, model.Err(400, "profileId and conversationId are required")
	}
	if err := r.member(p, q.ConversationID, false); err != nil {
		return nil, err
	}
	rows, err := r.db.Query("SELECT body FROM browser_sessions WHERE profile_id=? AND conversation_id=? ORDER BY rowid DESC LIMIT 32", q.ProfileID, q.ConversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []browserSession{}
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			return nil, err
		}
		var session browserSession
		if err = json.Unmarshal([]byte(body), &session); err != nil {
			return nil, err
		}
		session.LeaseOwner = ""
		session.Invocations = nil
		session.DataID = ""
		items = append(items, session)
	}
	return map[string]any{"items": items}, rows.Err()
}

func (r *Relay) browserOpen(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q browserOpenRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ProfileID == "" || q.ConversationID == "" {
		return nil, model.Err(400, "profileId and conversationId are required")
	}
	if q.TaskID != "" {
		return nil, model.Err(400, "taskId is reserved for an active model attempt")
	}
	session, err := r.browserCreateSession(ctx, p, q.ProfileID, q.ConversationID, "", q.Mode)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// browserCreateSession materializes one isolated browser for a profile and
// conversation. It never reuses a personal native session.
func (r *Relay) browserCreateSession(ctx context.Context, p model.Principal, profileID, conversationID, taskID, mode string) (browserSession, error) {
	if mode == "" {
		mode = "browser"
	}
	if mode != "browser" && mode != "desktop" {
		return browserSession{}, model.Err(400, "mode must be browser or desktop")
	}
	manager := r.browserManager()
	manager.create.Lock()
	defer manager.create.Unlock()
	manager.mu.Lock()
	closed, count := manager.closed, len(manager.sessions)
	manager.mu.Unlock()
	if closed {
		return browserSession{}, model.Err(503, "browser service is shutting down")
	}
	if !profileIdentifier(profileID) {
		return browserSession{}, model.Err(400, "invalid profile identifier")
	}
	r.mu.Lock()
	profile, err := r.profile(profileID)
	if err != nil {
		r.mu.Unlock()
		return browserSession{}, err
	}
	if profile.Archived {
		r.mu.Unlock()
		return browserSession{}, model.Err(409, "archived profiles cannot open a browser session")
	}
	if mode == "browser" && !profile.Capabilities.Browser || mode == "desktop" && !profile.Capabilities.Computer {
		r.mu.Unlock()
		return browserSession{}, model.Err(403, "the "+mode+" capability is not enabled for this profile")
	}
	r.mu.Unlock()
	if err = r.member(p, conversationID, false); err != nil {
		return browserSession{}, err
	}
	for _, runtime := range manager.list() {
		current := runtime.snapshot()
		if current.ProfileID == profileID && current.ConversationID == conversationID && current.Mode == mode {
			return current, nil
		}
	}
	if count >= 32 {
		return browserSession{}, model.Err(429, "at most 32 isolated browser/desktop sessions may be active")
	}
	session := browserSession{ID: randomID(), ProfileID: profileID, ConversationID: conversationID, TaskID: taskID, Mode: mode, DataID: browserDataID(profileID, conversationID, mode), State: browserStateOpening, Epoch: 1, CreatedAt: now(), UpdatedAt: now()}
	if err = r.storeBrowserSession(session); err != nil {
		return browserSession{}, err
	}
	worker, err := r.browserStartWorker(r.ctx, session)
	if err != nil {
		session.State = browserStateError
		session.LastError = truncate(redact(err.Error()), 1024)
		session.UpdatedAt = now()
		_ = r.storeBrowserSession(session)
		return browserSession{}, err
	}
	runtime := &browserRuntime{record: session, worker: worker, pending: map[string]bool{}}
	r.browserManager().put(runtime)
	session.State = browserStateActive
	session.UpdatedAt = now()
	runtime.store(session)
	if err = r.storeBrowserSession(session); err != nil {
		_ = worker.Close()
		manager.remove(session.ID)
		return browserSession{}, err
	}
	return session, nil
}

func (r *Relay) browserState(p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q browserSessionRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	session, err := r.loadBrowserSession(q.ID)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// browserLeaseActive reports whether a human still holds the session.
func browserLeaseActive(session browserSession) bool {
	if session.LeaseOwner == "" || session.LeaseExpiresAt == nil {
		return false
	}
	return session.LeaseExpiresAt.After(now())
}

// browserTakeover gives the operator exclusive control. The epoch advances
// first, then old epoch actions and approvals are invalidated, then the active
// input is cancelled and drained, and only then the human lease is recorded.
func (r *Relay) browserTakeover(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q browserEpochRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	runtime := r.browserManager().get(q.ID)
	if runtime == nil {
		return nil, model.Err(404, "browser session not found")
	}
	job, release, err := r.beginConsoleJob(ctx, p.ConsoleSessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	runtime.control.Lock()
	defer runtime.control.Unlock()
	runtime.mu.Lock()
	session := runtime.record
	if q.ExpectedEpoch != session.Epoch {
		runtime.mu.Unlock()
		return nil, model.Err(409, "stale browser epoch")
	}
	if session.State == browserStateClosed {
		runtime.mu.Unlock()
		return nil, model.Err(409, "browser session is closed")
	}
	if browserLeaseActive(session) && session.LeaseOwner != p.ConsoleSessionID {
		runtime.mu.Unlock()
		return nil, model.Err(403, "the browser takeover belongs to another console session")
	}
	previous := session
	session.Epoch++
	session.State = browserStateTakingOver
	session.LeaseOwner = ""
	session.LeaseExpiresAt = nil
	session.UpdatedAt = now()
	session, err = r.browserPersistEpochChange(previous, session, "browser takeover invalidated pending actions")
	if err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	runtime.record = session
	cancel := runtime.active
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// No new old-epoch dispatch can pass after the state transition. Wait for
	// the cancelled input to finish before acknowledging human control.
	runtime.dispatch.Lock()
	defer runtime.dispatch.Unlock()
	if err = job.Err(); err != nil {
		return nil, err
	}
	if worker, ok := runtime.worker.(*confinedBrowserWorker); ok && worker.isClosed() {
		runtime.worker, err = r.browserStartWorker(r.ctx, session)
		if err != nil {
			return nil, err
		}
	}
	r.browserCaptureSnapshot(runtime)
	session = runtime.snapshot()
	session.LeaseOwner = p.ConsoleSessionID
	expires := now().Add(browserLeaseDuration)
	session.LeaseExpiresAt = &expires
	session.UpdatedAt = now()
	if err = job.Err(); err != nil {
		return nil, err
	}
	if err = r.storeBrowserSession(session); err != nil {
		return nil, err
	}
	runtime.store(session)
	return session, nil
}

// browserRelease returns control to the model. The snapshot is refreshed, the
// epoch advances and stale actions are discarded.
func (r *Relay) browserRelease(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q browserEpochRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	runtime := r.browserManager().get(q.ID)
	if runtime == nil {
		return nil, model.Err(404, "browser session not found")
	}
	job, release, err := r.beginConsoleJob(ctx, p.ConsoleSessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	runtime.control.Lock()
	defer runtime.control.Unlock()
	runtime.dispatch.Lock()
	defer runtime.dispatch.Unlock()
	session := runtime.snapshot()
	if q.ExpectedEpoch != session.Epoch {
		return nil, model.Err(409, "stale browser epoch")
	}
	if session.State == browserStateClosed {
		return nil, model.Err(409, "browser session is closed")
	}
	if session.State != browserStateTakingOver {
		return nil, model.Err(409, "browser is not under human control")
	}
	if session.LeaseOwner != "" && session.LeaseOwner != p.ConsoleSessionID {
		return nil, model.Err(403, "the browser takeover belongs to another console session")
	}
	if err := job.Err(); err != nil {
		return nil, err
	}
	previous := session
	session.Epoch++
	session.State = browserStateActive
	session.LeaseOwner = ""
	session.LeaseExpiresAt = nil
	session.UpdatedAt = now()
	session, err = r.browserPersistEpochChange(previous, session, "browser release discarded stale actions")
	if err != nil {
		return nil, err
	}
	runtime.store(session)
	r.browserCaptureSnapshot(runtime)
	return runtime.snapshot(), nil
}

// browserPersistEpochChange stores the new session and invalidates the
// pending approvals of the previous epoch in one transaction.
func (r *Relay) browserPersistEpochChange(previous, next browserSession, reason string) (browserSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.Begin()
	if err != nil {
		return next, err
	}
	defer tx.Rollback()
	var stored string
	if err = tx.QueryRow("SELECT body FROM browser_sessions WHERE id=?", next.ID).Scan(&stored); err != nil {
		return next, err
	}
	var current browserSession
	if err = json.Unmarshal([]byte(stored), &current); err != nil {
		return next, err
	}
	next.Invocations = nil
	body, err := json.Marshal(next)
	if err != nil {
		return next, err
	}
	updated, err := tx.Exec("UPDATE browser_sessions SET state=?,epoch=?,body=? WHERE id=? AND epoch=?", next.State, next.Epoch, string(body), next.ID, previous.Epoch)
	if err != nil {
		return next, err
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return next, model.Err(409, "stale browser epoch")
	}
	if err = browserInvalidateInvocations(tx, current.Invocations, reason); err != nil {
		return next, err
	}
	if err = insertAutomationEvent(tx, "browser.epoch", next.ID, next.ProfileID, map[string]any{"epoch": next.Epoch, "state": next.State, "reason": reason}); err != nil {
		return next, err
	}
	if err = tx.Commit(); err != nil {
		return next, err
	}
	return next, nil
}

// browserInput dispatches one exact action. Human input requires the live
// takeover lease; model input stays blocked until an explicit release.
func (r *Relay) browserInput(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	var q browserInputRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	runtime := r.browserManager().get(q.ID)
	if runtime == nil {
		return nil, model.Err(404, "browser session not found")
	}
	return r.browserDispatch(ctx, runtime, q.Action, q.Epoch, q.FrameSeq, browserDispatchContext{human: true, principal: p})
}

// browserDispatchContext carries the trusted caller identity of one action.
type browserDispatchContext struct {
	human     bool
	principal model.Principal
	task      *model.Task
	broker    *toolBroker
	call      *brokerCall
}

// browserDispatch performs the epoch, frame and lease checks and then runs the
// action inside the confined worker.
func (r *Relay) browserDispatch(ctx context.Context, runtime *browserRuntime, action browserAction, epoch, frameSeq int, dispatch browserDispatchContext) (any, error) {
	if err := browserValidateModeAction(runtime.snapshot().Mode, action); err != nil {
		return nil, err
	}
	var release func()
	if dispatch.human {
		var err error
		ctx, release, err = r.beginConsoleJob(ctx, dispatch.principal.ConsoleSessionID)
		if err != nil {
			return nil, err
		}
		defer release()
	} else if dispatch.broker == nil || dispatch.call == nil {
		return nil, model.Err(403, "browser model input requires a claimed active broker invocation")
	}
	runtime.dispatch.Lock()
	defer runtime.dispatch.Unlock()
	session := runtime.snapshot()
	if dispatch.human && session.LeaseOwner != dispatch.principal.ConsoleSessionID {
		return nil, model.Err(403, "the browser takeover belongs to another console session")
	}
	if session.State == browserStateClosed {
		return nil, model.Err(409, "browser session is closed")
	}
	if epoch != session.Epoch {
		return nil, model.Err(409, "stale browser epoch")
	}
	if frameSeq != session.FrameSeq {
		return nil, model.Err(409, "stale browser frame")
	}
	if dispatch.human {
		if session.State != browserStateTakingOver {
			return nil, model.Err(409, "human control requires an active takeover")
		}
		if session.LeaseOwner != dispatch.principal.ConsoleSessionID {
			return nil, model.Err(403, "the browser takeover belongs to another console session")
		}
		if !browserLeaseActive(session) {
			return nil, model.Err(409, "the human browser lease expired; take over again")
		}
	} else {
		if session.State == browserStateTakingOver || browserLeaseActive(session) {
			return nil, model.Err(409, "human takeover is active; model input is blocked until an explicit release")
		}
		if session.State != browserStateActive {
			return nil, model.Err(409, "browser session is not active")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !dispatch.human {
		if err := r.browserClaimDispatch(runtime, action, epoch, frameSeq, dispatch); err != nil {
			return nil, err
		}
	}
	if action.Action == "open" || action.Action == "navigate" {
		if err := browserValidateURL(action.URL); err != nil {
			return nil, err
		}
	}
	if action.Action == "scroll" && action.Amount == 0 {
		action.Amount = 400
	}
	worker := runtime.worker
	if worker == nil {
		return nil, model.Err(503, "browser worker is not running")
	}
	workerAction := browserWorkerAction{Action: action.Action, URL: action.URL, Selector: action.Selector, Text: action.Text, Key: action.Key, Direction: action.Direction, Amount: action.Amount, Tab: action.Tab, Executable: action.Executable, X: action.X, Y: action.Y, WindowID: action.WindowID}
	if action.Action == "upload" {
		staged, err := r.browserStageUpload(ctx, session, action.AttachmentID, dispatch.principal)
		if err != nil {
			return nil, err
		}
		workerAction.Path = staged
		defer os.Remove(filepath.Join(r.browserStateDirectory(session.ID), "uploads", filepath.Base(staged)))
	}
	actionCtx, cancel := context.WithTimeout(ctx, browserActionTimeout)
	defer cancel()
	runtime.mu.Lock()
	runtime.active = cancel
	runtime.mu.Unlock()
	result, err := worker.Execute(actionCtx, workerAction)
	runtime.mu.Lock()
	runtime.active = nil
	runtime.mu.Unlock()
	if err != nil {
		return nil, err
	}
	runtime.mu.Lock()
	updated := runtime.record
	if updated.Epoch != epoch || actionCtx.Err() != nil {
		runtime.mu.Unlock()
		return nil, model.Err(409, "browser control changed during input; result discarded")
	}
	updated.FrameSeq++
	updated.UpdatedAt = now()
	if result.Text != "" {
		updated.SnapshotText = truncate(result.Text, browserSnapshotText)
		captured := now()
		updated.SnapshotAt = &captured
	}
	if result.Tabs > 0 {
		updated.Tabs = result.Tabs
	}
	runtime.record = updated
	runtime.frame = nil
	err = r.storeBrowserSession(updated)
	runtime.mu.Unlock()
	if err != nil {
		return nil, err
	}
	view := map[string]any{
		"id":        updated.ID,
		"epoch":     updated.Epoch,
		"frameSeq":  updated.FrameSeq,
		"state":     updated.State,
		"snapshot":  map[string]any{"text": updated.SnapshotText},
		"tabs":      updated.Tabs,
		"truncated": result.Truncated,
	}
	if result.Frame != "" {
		view["image"] = map[string]any{"mimeType": "image/png", "data": result.Frame}
	}
	if result.URL != "" {
		view["url"] = result.URL
	}
	if result.Title != "" {
		view["title"] = result.Title
	}
	if result.Download != "" {
		view["download"] = result.Download
		attachment, err := r.browserPublishDownload(ctx, session, result.Download, dispatch)
		if err != nil {
			return nil, err
		}
		view["attachment"] = attachment
	}
	return view, nil
}

// browserStateDirectory is the host path of the private browser state that is
// mounted inside the worker.
func (r *Relay) browserStateDirectory(sessionID string) string {
	dataID := sessionID
	if session, err := r.loadBrowserSession(sessionID); err == nil && session.DataID != "" {
		dataID = session.DataID
	}
	return filepath.Join(filepath.Dir(r.lock.Name()), "browser", "state", "state-"+dataID)
}

// browserStageUpload copies one authorized attachment into the private
// browser state. Only the daemon names the staged file.
func (r *Relay) browserStageUpload(ctx context.Context, session browserSession, attachmentID string, principal model.Principal) (string, error) {
	if attachmentID == "" {
		return "", model.Err(400, "an attachmentId is required for upload")
	}
	if !profileIdentifier(attachmentID) {
		return "", model.Err(400, "invalid attachment identifier")
	}
	attachment, reader, err := r.ReadAttachment(ctx, principal, attachmentID)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if attachment.Size > model.MaxAttachmentBytes {
		return "", model.Err(413, "browser upload exceeds attachment limit")
	}
	root, err := os.OpenRoot(r.browserStateDirectory(session.ID))
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err = root.Mkdir("uploads", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	name := randomID() + filepath.Ext(filepath.Base(attachment.Name))
	target := "uploads/" + name
	file, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(reader, model.MaxAttachmentBytes+1))
	if err != nil || size > model.MaxAttachmentBytes {
		_ = root.Remove(target)
		if err != nil {
			return "", err
		}
		return "", model.Err(413, "browser upload exceeds attachment limit")
	}
	if err = ctx.Err(); err != nil {
		_ = root.Remove(target)
		return "", err
	}
	if err = file.Sync(); err != nil {
		_ = root.Remove(target)
		return "", err
	}
	return "/home/worker/browser-data/uploads/" + name, nil
}

// browserPublishDownload turns a quarantined download into an attachment of
// the exact conversation. The file is never opened or executed.
func (r *Relay) browserPublishDownload(ctx context.Context, session browserSession, name string, dispatch browserDispatchContext) (model.Attachment, error) {
	base := filepath.Base(name)
	if base == "." || base != name || strings.ContainsAny(base, "/\\\x00") {
		return model.Attachment{}, model.Err(409, "the download name is invalid")
	}
	root, err := os.OpenRoot(r.browserStateDirectory(session.ID))
	if err != nil {
		return model.Attachment{}, err
	}
	defer root.Close()
	file, err := root.OpenFile("downloads/"+base, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return model.Attachment{}, model.Err(404, "the quarantined download is unavailable")
	}
	defer file.Close()
	defer root.Remove("downloads/" + base)
	info, err := file.Stat()
	if err != nil {
		return model.Attachment{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 20<<20 {
		return model.Attachment{}, model.Err(413, "the quarantined download must be a regular file of at most 20 MiB")
	}
	mediaType := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(base)) {
	case ".txt", ".md":
		mediaType = "text/plain; charset=utf-8"
	case ".json":
		mediaType = "application/json"
	case ".png":
		mediaType = "image/png"
	case ".jpg", ".jpeg":
		mediaType = "image/jpeg"
	case ".pdf":
		mediaType = "application/pdf"
	case ".zip":
		mediaType = "application/zip"
	case ".csv":
		mediaType = "text/csv; charset=utf-8"
	}
	if dispatch.human || dispatch.task == nil {
		return r.storeAttachment(ctx, dispatch.principal, session.ConversationID, base, mediaType, file, info.Size(), nil, "")
	}
	return r.storeTaskPublication(ctx, dispatch.principal, *dispatch.task, base, mediaType, file, info.Size(), "")
}

// browserCaptureSnapshot refreshes the visible snapshot without changing the
// epoch. It is best effort: a failing worker leaves the last snapshot.
func (r *Relay) browserCaptureSnapshot(runtime *browserRuntime) {
	worker := runtime.worker
	if worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, browserActionTimeout)
	defer cancel()
	action := "inspect"
	if runtime.snapshot().Mode == "desktop" {
		action = "snapshot"
	}
	result, err := worker.Execute(ctx, browserWorkerAction{Action: action})
	if err != nil {
		return
	}
	updated := runtime.snapshot()
	updated.FrameSeq++
	updated.UpdatedAt = now()
	if result.Text != "" {
		updated.SnapshotText = truncate(result.Text, browserSnapshotText)
		captured := now()
		updated.SnapshotAt = &captured
	}
	if result.Tabs > 0 {
		updated.Tabs = result.Tabs
	}
	runtime.store(updated)
	_ = r.storeBrowserSession(updated)
}

func (r *Relay) browserClose(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q browserSessionRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ID == "" {
		return nil, model.Err(400, "id is required")
	}
	runtime := r.browserManager().get(q.ID)
	if runtime == nil {
		return nil, model.Err(404, "browser session not found")
	}
	if err := r.member(p, runtime.snapshot().ConversationID, false); err != nil {
		return nil, err
	}
	if err := r.browserStopRuntime(runtime, "operator closed browser"); err != nil {
		return nil, err
	}
	return runtime.snapshot(), nil
}

// browserClearData removes the private browser application data of one
// profile and conversation. It requires an explicit confirmation.
func (r *Relay) browserClearData(ctx context.Context, p model.Principal, raw json.RawMessage) (any, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	var q browserClearRequest
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if !q.Confirm {
		return nil, model.Err(400, "confirm must be true to clear browser data")
	}
	if q.ProfileID == "" || q.ConversationID == "" {
		return nil, model.Err(400, "profileId and conversationId are required")
	}
	if !profileIdentifier(q.ProfileID) {
		return nil, model.Err(400, "invalid profile identifier")
	}
	if err := r.member(p, q.ConversationID, false); err != nil {
		return nil, err
	}
	manager := r.browserManager()
	manager.create.Lock()
	defer manager.create.Unlock()
	for _, runtime := range manager.list() {
		session := runtime.snapshot()
		if session.ProfileID == q.ProfileID && session.ConversationID == q.ConversationID {
			if err := r.browserStopRuntime(runtime, "operator cleared browser data"); err != nil {
				return nil, err
			}
		}
	}
	for _, mode := range []string{"browser", "desktop"} {
		dataID := browserDataID(q.ProfileID, q.ConversationID, mode)
		state := filepath.Join(filepath.Dir(r.lock.Name()), "browser", "state", "state-"+dataID)
		if err := os.RemoveAll(state); err != nil {
			return nil, err
		}
	}
	return map[string]any{"cleared": true}, nil
}

// BrowserFrame returns the current pixels of one browser session for the
// console viewer. It never renders remote HTML.
func (r *Relay) BrowserFrame(ctx context.Context, p model.Principal, id string) ([]byte, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, model.Err(404, "browser session not found")
	}
	runtime := r.browserManager().get(id)
	if runtime == nil {
		return nil, model.Err(404, "browser session not found")
	}
	if err := r.member(p, runtime.snapshot().ConversationID, false); err != nil {
		return nil, err
	}
	job, release, err := r.beginConsoleJob(ctx, p.ConsoleSessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	runtime.dispatch.Lock()
	defer runtime.dispatch.Unlock()
	if err = job.Err(); err != nil {
		return nil, err
	}
	session := runtime.snapshot()
	if session.State == browserStateClosed || runtime.worker == nil {
		return nil, model.Err(409, "browser session is closed")
	}
	if len(runtime.frame) != 0 && time.Since(runtime.lastFrame) < 500*time.Millisecond {
		return bytes.Clone(runtime.frame), nil
	}
	frameContext, cancel := context.WithTimeout(job, browserActionTimeout)
	defer cancel()
	frame, err := runtime.worker.Frame(frameContext)
	if err != nil {
		return nil, err
	}
	if err = browserValidatePNG(frame, browserFrameLimit); err != nil {
		return nil, err
	}
	if err = job.Err(); err != nil {
		return nil, err
	}
	runtime.frame, runtime.lastFrame = frame, now()
	return bytes.Clone(frame), nil
}

// ---- broker entry --------------------------------------------------------

func browserToolAction(args BrowserToolArgs) browserAction {
	return browserAction{Action: args.Action, URL: args.URL, Selector: args.Selector, Text: args.Text, Key: args.Key, Direction: args.Direction, Amount: args.Amount, Tab: args.Tab, AttachmentID: args.AttachmentID, Executable: args.Executable, X: args.X, Y: args.Y, WindowID: args.WindowID}
}

// browserPrepareTool pins the exact session/frame/epoch before approval. The
// session is created using the admitted task principal, never an admin surrogate.
func (r *Relay) browserPrepareTool(ctx context.Context, b *toolBroker, args BrowserToolArgs, computer bool) (BrowserToolArgs, error) {
	mode := "browser"
	if computer {
		mode = "desktop"
	}
	if err := browserValidateModeAction(mode, browserToolAction(args)); err != nil {
		return args, err
	}
	if b == nil || b.relay != r {
		return args, model.Err(403, "active browser broker required")
	}
	r.mu.Lock()
	task, principal, _, err := b.activeLocked()
	if err == nil && r.toolBrokers[task.ID] != b {
		err = model.Err(403, "browser broker is no longer registered")
	}
	r.mu.Unlock()
	if err != nil {
		return args, err
	}
	var session browserSession
	if args.SessionID == "" {
		session, err = r.browserCreateSession(r.ctx, principal, b.snapshot.Profile.ID, task.ConversationID, task.ID, mode)
	} else {
		runtime := r.browserManager().get(args.SessionID)
		if runtime == nil {
			return args, model.Err(404, "browser session not found")
		}
		session = runtime.snapshot()
	}
	if err != nil {
		return args, err
	}
	if session.ProfileID != b.snapshot.Profile.ID || session.ConversationID != task.ConversationID || session.Mode != mode {
		return args, model.Err(403, "browser session does not belong to this profile conversation and tool")
	}
	if session.State != browserStateActive || session.LeaseOwner != "" {
		return args, model.Err(409, "human takeover is active; explicitly release before model input")
	}
	args.SessionID, args.Epoch, args.FrameSeq = session.ID, session.Epoch, session.FrameSeq
	return args, nil
}

// browserExecuteTool accepts only the in-memory call already claimed by the
// broker. Caller-provided task/approval rows confer no authority.
func (r *Relay) browserExecuteTool(ctx context.Context, b *toolBroker, call *brokerCall, args BrowserToolArgs, computer bool) (model.ToolResult, error) {
	var zero model.ToolResult
	if b == nil || b.relay != r || call == nil {
		return zero, model.Err(403, "claimed browser broker invocation required")
	}
	r.mu.Lock()
	task, principal, _, err := b.activeLocked()
	r.mu.Unlock()
	if err != nil {
		return zero, err
	}
	runtime := r.browserManager().get(args.SessionID)
	if runtime == nil {
		return zero, model.Err(404, "browser session not found")
	}
	session := runtime.snapshot()
	mode := "browser"
	if computer {
		mode = "desktop"
	}
	if session.Mode != mode || session.ProfileID != b.snapshot.Profile.ID || session.ConversationID != task.ConversationID {
		return zero, model.Err(403, "browser session binding mismatch")
	}
	value, err := r.browserDispatch(ctx, runtime, browserToolAction(args), args.Epoch, args.FrameSeq, browserDispatchContext{principal: principal, task: &task, broker: b, call: call})
	if err != nil {
		return zero, err
	}
	return toolJSON(value)
}

func (r *Relay) browserClaimDispatch(runtime *browserRuntime, action browserAction, epoch, frame int, dispatch browserDispatchContext) error {
	b, call := dispatch.broker, dispatch.call
	sessionID := runtime.snapshot().ID
	r.mu.Lock()
	defer r.mu.Unlock()
	task, principal, _, err := b.activeLocked()
	if err != nil {
		return err
	}
	if r.toolBrokers[task.ID] != b || b.calls[call.invocation.CallID] != call || call.done || call.action == nil || call.invocation.State != "claimed" || principal != dispatch.principal {
		return model.Err(403, "browser invocation is not the active claimed broker call")
	}
	args, ok := call.action.args.(BrowserToolArgs)
	if !ok || args.SessionID != sessionID || args.Epoch != epoch || args.FrameSeq != frame || browserToolAction(args) != action {
		return model.Err(409, "browser action differs from the exact approved action")
	}
	readOnly := action.Action == "inspect" || action.Action == "screenshot" || action.Action == "snapshot"
	if !readOnly && (!call.action.mutation || !call.action.approval || call.approvalID == "" || call.owner == "") {
		return model.Err(403, "one-call browser mutation approval is required")
	}
	var state string
	if err = r.db.QueryRow("SELECT state FROM tool_invocations WHERE id=? AND task_id=? AND attempt=? AND action_hash=?", call.invocation.ID, task.ID, b.attempt, call.invocation.ActionHash).Scan(&state); err != nil || state != "claimed" {
		return model.Err(409, "browser invocation claim is stale")
	}
	if !readOnly {
		var status, expires string
		if err = r.db.QueryRow("SELECT status,expires_at FROM tool_approvals WHERE id=? AND invocation_id=?", call.approvalID, call.invocation.ID).Scan(&status, &expires); err != nil {
			return err
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, expires)
		if parseErr != nil || status != "claimed" || !deadline.After(now()) {
			return model.Err(409, "browser approval expired or was invalidated")
		}
	}
	if runtime.dispatched == nil {
		runtime.dispatched = map[string]bool{}
	}
	if runtime.dispatched[call.invocation.ID] {
		return model.Err(409, "browser invocation cannot dispatch twice")
	}
	if len(runtime.dispatched) >= 256 {
		return model.Err(409, "browser invocation limit reached; close and reopen this session")
	}
	runtime.dispatched[call.invocation.ID] = true
	return nil
}

// browserRegisterInvocation records a pending browser invocation so a human
// takeover can invalidate it before it reaches the worker.
func (r *Relay) browserRegisterInvocation(sessionID, invocationID string, epoch int) error {
	if sessionID == "" || invocationID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var body string
	if err := r.db.QueryRow("SELECT body FROM browser_sessions WHERE id=?", sessionID).Scan(&body); err != nil {
		return nil
	}
	var session browserSession
	if err := json.Unmarshal([]byte(body), &session); err != nil {
		return nil
	}
	if session.Epoch != epoch {
		return model.Err(409, "stale browser epoch")
	}
	for _, existing := range session.Invocations {
		if existing == invocationID {
			return nil
		}
	}
	session.Invocations = append(session.Invocations, invocationID)
	if len(session.Invocations) > 64 {
		session.Invocations = session.Invocations[len(session.Invocations)-64:]
	}
	updated, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = r.db.Exec("UPDATE browser_sessions SET body=? WHERE id=?", string(updated), sessionID)
	return err
}

// ---- confined worker -----------------------------------------------------

func (r *Relay) browserRoot() (string, error) {
	base := filepath.Dir(r.lock.Name())
	dir := filepath.Join(base, "browser")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("browser directory must be private")
	}
	return dir, nil
}

// browserArtifactsPath finds the pinned Playwright browser artifacts inside
// the operator reviewed runtime manifest.
func browserArtifactsPath(config sandbox.Config) string {
	for _, path := range config.RuntimePaths {
		if filepath.Base(path) == "ms-playwright" {
			return path
		}
		if info, err := os.Stat(filepath.Join(path, "ms-playwright")); err == nil && info.IsDir() {
			return filepath.Join(path, "ms-playwright")
		}
	}
	return ""
}

// browserBridgeScript resolves the reviewed bridge script and checks that it
// is covered by a mounted runtime path.
func browserBridgeScript(config sandbox.Config) (string, error) {
	script := config.Executables["browser-bridge"]
	if script == "" {
		return "", model.Err(503, "configure runtime.configure executables.browser-bridge with the reviewed bridges/browser/bridge.mjs path")
	}
	covered := false
	for _, path := range config.RuntimePaths {
		if script == path || strings.HasPrefix(script, path+"/") {
			covered = true
			break
		}
	}
	if !covered {
		return "", model.Err(503, "the browser bridge must live under a runtime.configure runtimePaths entry so the worker can read it")
	}
	return script, nil
}

// browserStartWorker launches the confined browser bridge with the mounted
// egress socket and the private browser profile.
func (r *Relay) browserStartWorker(ctx context.Context, session browserSession) (browserWorker, error) {
	config, _, err := r.runtimeConfig()
	if err != nil {
		return nil, err
	}
	status := sandbox.Check(ctx, config)
	if status.State != "ready" {
		return nil, model.Err(503, "confined runtime "+status.State+": "+strings.Join(status.Prerequisites, "; "))
	}
	node := config.Executables["node"]
	if node == "" {
		return nil, model.Err(503, "configure runtime.configure executables.node with the Node.js runtime used by the browser bridge")
	}
	script, err := browserBridgeScript(config)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	profile, err := r.profile(session.ProfileID)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	workspace, err := r.prepareWorkspace(ctx, profile.WorkspaceID)
	if err != nil {
		return nil, err
	}
	root, err := r.browserRoot()
	if err != nil {
		workspace.Close()
		return nil, err
	}
	state, err := sandbox.OpenPrivateState(filepath.Join(root, "state"), session.ID)
	if err != nil {
		workspace.Close()
		return nil, sandboxError(err)
	}
	socketDir := filepath.Join(root, session.ID)
	if err = os.MkdirAll(socketDir, 0700); err != nil {
		workspace.Close()
		return nil, err
	}
	scope := netpolicy.Scope{ProfileID: session.ProfileID}
	proxy, err := r.networkProxy(ctx, filepath.Join(socketDir, "egress.sock"), scope, netpolicy.ProxyOptions{
		MaxStreams:        8,
		MaxStreamBytes:    32 << 20,
		MaxStreamDuration: 5 * time.Minute,
		Active: func(ctx context.Context) error {
			runtime := r.browserManager().get(session.ID)
			if runtime == nil {
				return errors.New("browser session is no longer active")
			}
			current := runtime.snapshot()
			if current.State == browserStateClosed {
				return errors.New("browser session is closed")
			}
			return nil
		},
	})
	if err != nil {
		workspace.Close()
		return nil, model.Err(503, "the browser egress proxy is unavailable: "+err.Error())
	}
	argv := []string{node, script, "--mode", "browser", "--egress-socket", "/run/meshra/egress.sock", "--state-dir", "/home/worker/browser-data"}
	env := map[string]string{}
	if artifacts := browserArtifactsPath(config); artifacts != "" {
		env["PLAYWRIGHT_BROWSERS_PATH"] = artifacts
	}
	prepared, err := sandbox.Prepare(ctx, config, sandbox.Request{
		Workspace:    workspace,
		Argv:         argv,
		Kind:         sandbox.Browser,
		Env:          env,
		SocketMounts: []sandbox.SocketMount{{Source: proxy.Path(), Target: "/run/meshra/egress.sock", Purpose: "egress"}},
		State:        state,
	})
	if err != nil {
		proxy.Close()
		workspace.Close()
		return nil, sandboxError(err)
	}
	worker, err := newConfinedBrowserWorker(ctx, prepared, workspace, proxy, session)
	if err != nil {
		prepared.Close()
		proxy.Close()
		workspace.Close()
		return nil, err
	}
	return worker, nil
}

// confinedBrowserWorker speaks the bounded JSONL control protocol with the
// bridge process inside the confined worker.
type confinedBrowserWorker struct {
	prepared  *sandbox.Prepared
	workspace *sandbox.Snapshot
	proxy     *netpolicy.Proxy
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	mu        sync.Mutex
	sequence  int
	closed    bool
}

func newConfinedBrowserWorker(ctx context.Context, prepared *sandbox.Prepared, workspace *sandbox.Snapshot, proxy *netpolicy.Proxy, session browserSession) (*confinedBrowserWorker, error) {
	stdin, err := prepared.Cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := prepared.Cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	prepared.Cmd.Stderr = io.Discard
	if err = prepared.Start(); err != nil {
		return nil, sandboxError(err)
	}
	worker := &confinedBrowserWorker{prepared: prepared, workspace: workspace, proxy: proxy, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 64<<10)}
	ready, err := worker.readFrame(ctx, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if ready.Type != "ready" {
		return nil, model.Err(503, "the browser bridge did not report readiness: "+truncate(redact(ready.Error), 512))
	}
	return worker, nil
}

// Execute sends one exact action and waits for its answer.
func (w *confinedBrowserWorker) Execute(ctx context.Context, action browserWorkerAction) (browserResult, error) {
	result, _, err := w.execute(ctx, action, browserModelImageLimit)
	return result, err
}

func (w *confinedBrowserWorker) execute(ctx context.Context, action browserWorkerAction, imageLimit int) (browserResult, []byte, error) {
	var result browserResult
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return result, nil, model.Err(503, "the browser worker is closed")
	}
	w.sequence++
	id := fmt.Sprintf("%d", w.sequence)
	payload, err := json.Marshal(map[string]any{"id": id, "action": action.Action, "args": action})
	if err != nil {
		w.mu.Unlock()
		return result, nil, err
	}
	if _, err = w.stdin.Write(append(payload, '\n')); err != nil {
		w.mu.Unlock()
		return result, nil, model.Err(503, "the browser bridge stopped accepting actions")
	}
	w.mu.Unlock()
	frame, err := w.readFrame(ctx, browserActionTimeout)
	if err != nil {
		return result, nil, err
	}
	if frame.ID != id {
		_ = w.Close()
		return result, nil, model.Err(503, "the browser bridge replied to a different action")
	}
	if !frame.OK {
		message := frame.Error
		if message == "" {
			message = "the browser action failed"
		}
		return result, nil, model.Err(409, truncate(redact(message), 512))
	}
	result = frame.Result
	if result.Text != "" && len(result.Text) > browserSnapshotText {
		result.Text = result.Text[:browserSnapshotText]
		result.Truncated = true
	}
	var pixels []byte
	if result.Frame != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(result.Frame)
		if decodeErr != nil {
			return result, nil, model.Err(503, "browser bridge sent invalid image data")
		}
		result.FrameBytes = len(decoded)
		if len(decoded) > imageLimit && imageLimit == browserModelImageLimit {
			result.Frame, result.Truncated = "", true
		} else {
			if err := browserValidatePNG(decoded, imageLimit); err != nil {
				return result, nil, err
			}
			pixels = decoded
		}
	}
	return result, pixels, nil
}

// Frame returns the current pixels for the console viewer.
func (w *confinedBrowserWorker) Frame(ctx context.Context) ([]byte, error) {
	_, pixels, err := w.execute(ctx, browserWorkerAction{Action: "frame"}, browserFrameLimit)
	if err != nil {
		return nil, err
	}
	if len(pixels) == 0 {
		return nil, model.Err(503, "browser worker returned no frame")
	}
	return pixels, nil
}

func (w *confinedBrowserWorker) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	err := w.prepared.Close()
	if w.proxy != nil {
		_ = w.proxy.Close()
	}
	if w.workspace != nil {
		_ = w.workspace.Close()
	}
	return err
}

// closeBrowserSessions stops every browser worker. The relay calls it while
// shutting down so no confined worker outlives the daemon.
func (r *Relay) closeBrowserSessions() error {
	manager := r.browserManager()
	manager.create.Lock()
	defer manager.create.Unlock()
	manager.mu.Lock()
	manager.closed = true
	manager.mu.Unlock()
	var first error
	for _, runtime := range manager.list() {
		first = errors.Join(first, r.browserStopRuntime(runtime, "relay shutdown"))
	}
	return first
}
