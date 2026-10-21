package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"meshra/internal/model"
	_ "modernc.org/sqlite"
)

//go:embed migrations/001.sql
var migration string

//go:embed migrations/002.sql
var migrationV2 string

//go:embed migrations/003.sql
var migrationV3 string

//go:embed migrations/004.sql
var migrationV4 string

//go:embed migrations/005.sql
var migrationV5 string

//go:embed migrations/006.sql
var migrationV6 string

//go:embed migrations/007.sql
var migrationV7 string

//go:embed migrations/008.sql
var migrationV8 string

//go:embed migrations/009.sql
var migrationV9 string

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type slot struct {
	lastUsed   time.Time
	runtime    model.Runtime
	cancel     context.CancelFunc
	turnCancel context.CancelFunc
	taskID     string
	repo       string
}
type Relay struct {
	accountMu  sync.Mutex
	mu         sync.Mutex
	lifecycle  sync.Mutex
	db         *sql.DB
	lock       *os.File
	admin      string
	adapters   map[string]model.Adapter
	slots      map[string]*slot
	repos      map[string]bool
	wake       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	closed     bool
	mcpURL     string
	owner      string
	options    Options
	settings   model.PersonalSettings
	catalogMu  sync.Mutex
	quotaCache map[string]model.QuotaSnapshot
	logger     *slog.Logger
	nextPrune  time.Time
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func tokenHash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func now() time.Time            { return time.Now().UTC() }
func Open(dir string, adapters map[string]model.Adapter) (_ *Relay, err error) {
	return OpenWithOptions(dir, adapters, Options{})
}

func OpenWithOptions(dir string, adapters map[string]model.Adapter, options Options) (_ *Relay, err error) {
	options, err = options.defaults()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("data directory already owned by another daemon: %w", err)
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	tokenPath := filepath.Join(dir, "admin.token")
	b, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte(randomID() + randomID())
		err = os.WriteFile(tokenPath, b, 0600)
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) < 32 {
		return nil, errors.New("admin.token must contain at least 32 characters")
	}
	if err = os.Chmod(tokenPath, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "meshra.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	if err = migrate(db); err != nil {
		return nil, err
	}
	if err = os.Chmod(filepath.Join(dir, "meshra.db"), 0600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{db: db, lock: lock, admin: strings.TrimSpace(string(b)), adapters: adapters, slots: map[string]*slot{}, quotaCache: map[string]model.QuotaSnapshot{}, repos: map[string]bool{}, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, owner: randomID(), options: options, logger: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	if err = r.loadPersonalSettings(); err != nil {
		cancel()
		return nil, err
	}
	if err = r.recover(); err != nil {
		cancel()
		return nil, err
	}
	if _, err = r.db.Exec("UPDATE run_usage SET state='interrupted' WHERE state='running'"); err != nil {
		cancel()
		return nil, err
	}
	r.wg.Add(1)
	go r.schedule()
	r.wg.Add(1)
	go r.reapIdle()
	return r, nil
}
func (r *Relay) SetMCPURL(url string) { r.mu.Lock(); defer r.mu.Unlock(); r.mcpURL = url }
func (r *Relay) Authenticate(token string) (model.Principal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(r.admin)) == 1 {
		return model.Principal{Admin: true}, nil
	}
	var p model.Principal
	err := r.db.QueryRow("SELECT agent_id,session_id,task_id FROM credentials WHERE hash=?", tokenHash(token)).Scan(&p.AgentID, &p.SessionID, &p.TaskID)
	if err != nil {
		return p, model.Err(401, "invalid bearer credential")
	}
	if p.TaskID != "" {
		t, e := r.task(p.TaskID)
		if e != nil || model.Terminal(t.State) {
			return model.Principal{}, model.Err(401, "turn credential expired")
		}
	}
	return p, nil
}
func requireAdmin(p model.Principal) error {
	if !p.Admin {
		return model.Err(403, "administrator credential required")
	}
	return nil
}
func (r *Relay) agent(id string) (model.Agent, error) {
	var a model.Agent
	var b string
	err := r.db.QueryRow("SELECT body FROM agents WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return a, model.Err(404, "agent not found: "+id)
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(b), &a)
	return a, err
}
func (r *Relay) saveAgent(a model.Agent) error {
	a.LastSeen = now()
	a.Error = redact(a.Error)
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = r.db.Exec("UPDATE agents SET body=? WHERE id=?", string(b), a.ID)
	return err
}
func (r *Relay) agents() ([]model.Agent, error) {
	rows, err := r.db.Query("SELECT body FROM agents ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Agent{}
	for rows.Next() {
		var b string
		var a model.Agent
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (r *Relay) Create(p model.Principal, c model.CreateAgent) (model.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(p, c)
}
func (r *Relay) createLocked(p model.Principal, c model.CreateAgent) (model.Agent, error) {
	if r.closed {
		return model.Agent{}, model.Err(503, "relay is stopping")
	}
	var a model.Agent
	if err := requireAdmin(p); err != nil {
		return a, err
	}
	if !namePattern.MatchString(c.Name) {
		return a, model.Err(400, "name must match [a-z][a-z0-9_-]{0,62}; IDs are lowercase")
	}
	if c.Harness != "omp" && c.Harness != "claude" && c.Harness != "codex" && c.Harness != "opencode" {
		return a, model.Err(400, "unsupported harness")
	}
	if c.Concurrency != 0 && c.Concurrency != 1 {
		return a, model.Err(400, "MVP concurrency must be one")
	}
	cwd, err := canonicalWorkspace(c.Cwd)
	if err != nil {
		return a, err
	}
	for _, s := range []string{c.Config.Model, c.Config.Provider} {
		if len(s) > 200 || !safeRegistryText(s) || strings.Contains(s, "sk-") || redact(s) != s {
			return a, model.Err(400, "model/provider must be identifiers, not credentials")
		}
	}
	for _, op := range c.Policy.Operations {
		if !validOperation(op) {
			return a, model.Err(400, "invalid permitted operation")
		}
	}
	for _, op := range c.Policy.InboundOperations {
		if op != "send" && op != "request" && op != "*" {
			return a, model.Err(400, "inbound operations must be send, request, or *")
		}
	}
	for _, id := range c.Policy.Destinations {
		if id != "*" && !validAgentID(id) {
			return a, model.Err(400, "invalid allowed destination")
		}
	}
	for _, id := range c.Policy.Senders {
		if id != "*" && !validAgentID(id) {
			return a, model.Err(400, "invalid allowed sender")
		}
	}
	if c.Policy.WorktreeWrites {
		return a, model.Err(400, "worktree writes are not supported by current adapter approval enforcement")
	}
	id := c.Harness + ":" + c.Name
	var exists int
	if err = r.db.QueryRow("SELECT count(*) FROM retired_agents WHERE id=?", id).Scan(&exists); err != nil {
		return a, err
	}
	if exists > 0 {
		return a, model.Err(409, "removed identity cannot be reused; choose a new name")
	}
	display := c.DisplayName
	if display == "" {
		display = c.Name
	}
	if len(display) > 200 || !safeRegistryText(display) || redact(display) != display {
		return a, model.Err(400, "display name must be at most 200 bytes without control characters or recognized credentials")
	}
	caps := map[string]bool{}
	if ad, ok := r.adapters[c.Harness]; ok {
		for k, v := range ad.Capabilities {
			caps[k] = v
		}
	}
	a = model.Agent{ManagedChat: c.ManagedChat, SchemaVersion: 1, ID: id, Harness: c.Harness, Name: c.Name, DisplayName: display, Cwd: cwd, Config: c.Config, Policy: c.Policy, Concurrency: 1, State: "registered", LastSeen: now(), Capabilities: caps}
	b, _ := json.Marshal(a)
	_, err = r.db.Exec("INSERT INTO agents(id,body) VALUES(?,?)", id, string(b))
	if err != nil {
		if _, e := r.agent(id); e == nil {
			return model.Agent{}, model.Err(409, "agent already exists: "+id)
		}
		return model.Agent{}, err
	}
	return a, nil
}
func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v || x == "*" {
			return true
		}
	}
	return false
}
func (r *Relay) permit(p model.Principal, op string) error {
	if p.Admin {
		return nil
	}
	a, err := r.agent(p.AgentID)
	if err != nil {
		return model.Err(401, "caller identity no longer exists")
	}
	if !contains(a.Policy.Operations, op) {
		return model.Err(403, "operation not permitted: "+op)
	}
	return nil
}
func (r *Relay) Agents(p model.Principal) ([]model.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.permit(p, "list"); err != nil {
		return nil, err
	}
	return r.agents()
}
func (r *Relay) Agent(p model.Principal, id string) (model.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.permit(p, "status"); err != nil {
		return model.Agent{}, err
	}
	a, err := r.agent(id)
	if err != nil {
		return a, err
	}
	if !p.Admin && p.AgentID != id {
		caller, e := r.agent(p.AgentID)
		if e != nil || !contains(caller.Policy.Destinations, id) {
			return model.Agent{}, model.Err(403, "destination status not permitted")
		}
	}
	return a, nil
}
func (r *Relay) Token(p model.Principal, id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := requireAdmin(p); err != nil {
		return "", err
	}
	a, err := r.agent(id)
	if err != nil {
		return "", err
	}
	return r.issue(id, a.SessionID, "")
}
func (r *Relay) issue(id, session, task string) (string, error) {
	token := randomID() + randomID()
	_, err := r.db.Exec("INSERT INTO credentials(hash,agent_id,session_id,task_id) VALUES(?,?,?,?)", tokenHash(token), id, session, task)
	return token, err
}
func (r *Relay) Remove(p model.Principal, id string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.agent(id); err != nil {
		return err
	}
	if _, ok := r.slots[id]; ok {
		return model.Err(409, "stop agent before removing it")
	}
	ts, err := r.tasks()
	if err != nil {
		return err
	}
	for _, t := range ts {
		if !model.Terminal(t.State) && (t.To == id || t.From == id) {
			return model.Err(409, "cancel outstanding tasks before removing agent")
		}
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM credentials WHERE agent_id=?", id); err != nil {
		return err
	}
	for _, table := range []string{"conversation_participants", "context_acl"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE agent_id=?", id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM agents WHERE id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO retired_agents(id) VALUES(?)", id); err != nil {
		return err
	}
	return tx.Commit()
}
func repository(cwd string) string {
	for p := cwd; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return cwd
}
func (r *Relay) Action(p model.Principal, id, action string) (model.Agent, error) {
	if err := requireAdmin(p); err != nil {
		return model.Agent{}, err
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if action != "start" && action != "stop" && action != "restart" {
		return model.Agent{}, model.Err(400, "action must be start, stop, or restart")
	}
	if action == "stop" || action == "restart" {
		if err := r.stop(id); err != nil {
			return model.Agent{}, err
		}
		if action == "stop" {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.agent(id)
		}
	}
	r.mu.Lock()
	a, err := r.agent(id)
	if err != nil {
		r.mu.Unlock()
		return a, err
	}
	if r.closed {
		r.mu.Unlock()
		return a, model.Err(503, "relay is shutting down")
	}
	if _, ok := r.slots[id]; ok {
		r.mu.Unlock()
		return a, nil
	}
	if len(r.slots) >= r.settings.MaxStartedAgents {
		r.mu.Unlock()
		return a, model.Err(429, "started agent limit reached; stop an idle agent or raise the limit in Settings")
	}
	ad, ok := r.adapters[a.Harness]
	if !ok || ad.Factory == nil {
		r.mu.Unlock()
		return a, model.Err(503, "adapter unavailable: "+a.Harness)
	}
	if err = validateWorkspace(a); err != nil {
		r.mu.Unlock()
		return a, err
	}
	if a.ManagedChat {
		// Managed identities are readiness slots, not durable model histories.
		// Idle Codex threads may have no rollout to resume after a restart.
		a.SessionID = ""
		a.Ephemeral = true
	}
	ctx, cancel := context.WithCancel(r.ctx)
	s := &slot{cancel: cancel, repo: repository(a.Cwd), lastUsed: now()}
	r.slots[id] = s
	a.State = "starting"
	a.Error = ""
	if err = r.saveAgent(a); err != nil {
		delete(r.slots, id)
		cancel()
		r.mu.Unlock()
		return a, err
	}
	r.mu.Unlock()
	runtime, err := ad.Factory(ctx, a, func(kind, data string) {
		if kind == "session" {
			r.mu.Lock()
			defer r.mu.Unlock()
			current, e := r.agent(id)
			if e == nil {
				current.SessionID = data
				_ = r.saveAgent(current)
			}
		}
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	a, _ = r.agent(id)
	if err != nil {
		cancel()
		delete(r.slots, id)
		a.State = "error"
		a.Error = "adapter start failed; check harness installation and native authentication"
		_ = r.saveAgent(a)
		return a, model.Err(503, fmt.Sprintf("adapter %s failed to start: %v", a.Harness, err))
	}
	s.runtime = runtime
	a.SessionID = runtime.SessionID()
	a.State = "idle"
	if err = r.saveAgent(a); err != nil {
		cancel()
		runtime.Close()
		delete(r.slots, id)
		return a, err
	}
	r.signal()
	return a, nil
}
func (r *Relay) stop(id string) error {
	r.mu.Lock()
	a, err := r.agent(id)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	s, ok := r.slots[id]
	if ok {
		s.cancel()
		if s.turnCancel != nil {
			s.turnCancel()
		}
		if s.taskID != "" {
			t, e := r.task(s.taskID)
			if e == nil && !model.Terminal(t.State) {
				t.State = "cancelled"
				t.ErrorCategory = "cancelled"
				t.Ambiguous = true
				t.Error = "agent stopped; execution may have external effects"
				err = r.transition(t, "cancelled", t.Error)
			}
		}
	}
	a.State = "stopped"
	a.Error = ""
	if e := r.saveAgent(a); err == nil {
		err = e
	}
	r.mu.Unlock()
	if ok && s.runtime != nil {
		if e := s.runtime.Close(); err == nil {
			err = e
		}
	}
	r.mu.Lock()
	if r.slots[id] == s {
		delete(r.slots, id)
	}
	r.mu.Unlock()
	return err
}
func (r *Relay) Close() error {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.cancel()
	for _, s := range r.slots {
		s.cancel()
		if s.turnCancel != nil {
			s.turnCancel()
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for id, s := range r.slots {
		if s.runtime != nil {
			if e := s.runtime.Close(); first == nil {
				first = e
			}
		}
		a, e := r.agent(id)
		if e == nil {
			a.State = "stopped"
			if e = r.saveAgent(a); first == nil {
				first = e
			}
		}
	}
	if e := r.db.Close(); first == nil {
		first = e
	}
	if e := r.lock.Close(); first == nil {
		first = e
	}
	return first
}
