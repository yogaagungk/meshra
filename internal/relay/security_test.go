package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestRecipientAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		senders    []string
		operations []string
		op         string
		admin      bool
		denied     bool
	}{
		{name: "legacy omitted lists"},
		{name: "explicit empty senders", senders: []string{}, denied: true},
		{name: "different sender", senders: []string{"omp:other"}, denied: true},
		{name: "exact sender", senders: []string{"omp:origin"}},
		{name: "wildcard sender", senders: []string{"*"}},
		{name: "explicit empty operations", operations: []string{}, denied: true},
		{name: "send denied by request only", operations: []string{"request"}, denied: true},
		{name: "request allowed", operations: []string{"request"}, op: "request"},
		{name: "wildcard operation", operations: []string{"*"}},
		{name: "administrator bypass", senders: []string{}, operations: []string{}, admin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := openTest(t, nil)
			origin := createTest(t, r, "omp", "origin")
			dest, err := r.Create(admin, model.CreateAgent{Harness: "claude", Name: "recipient", Cwd: t.TempDir(), Policy: model.Policy{Senders: tc.senders, InboundOperations: tc.operations}})
			if err != nil {
				t.Fatal(err)
			}
			p := model.Principal{AgentID: origin.ID}
			if tc.admin {
				p = admin
			}
			_, err = r.Send(p, model.SendRequest{From: origin.ID, To: dest.ID, Type: tc.op, Text: "private message"})
			if tc.denied {
				requireStatus(t, err, 403)
			} else if err != nil {
				t.Fatal(err)
			}
			var count int
			if err = r.db.QueryRow("SELECT count(*) FROM audit").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.denied {
				want = 1
			}
			if count != want {
				t.Fatalf("audit count = %d, want %d", count, want)
			}
		})
	}
}

func TestInboundPolicyJSONRoundTrip(t *testing.T) {
	r, _ := openTest(t, nil)
	for _, tc := range []struct {
		name   string
		policy string
		empty  bool
	}{
		{name: "omitted", policy: `{}`},
		{name: "null", policy: `{"senders":null,"inboundOperations":null}`},
		{name: "empty", policy: `{"senders":[],"inboundOperations":[]}`, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var policy model.Policy
			if err := json.Unmarshal([]byte(tc.policy), &policy); err != nil {
				t.Fatal(err)
			}
			a, err := r.Create(admin, model.CreateAgent{Harness: "omp", Name: tc.name, Cwd: t.TempDir(), Policy: policy})
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := r.Agent(admin, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (persisted.Policy.Senders != nil) != tc.empty || (persisted.Policy.InboundOperations != nil) != tc.empty {
				t.Fatalf("nil/empty semantics changed in SQLite: %+v", persisted.Policy)
			}
			body, err := json.Marshal(persisted)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Policy map[string]json.RawMessage `json:"policy"`
			}
			if err = json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			want := "null"
			if tc.empty {
				want = "[]"
			}
			for _, key := range []string{"senders", "inboundOperations"} {
				if string(wire.Policy[key]) != want {
					t.Fatalf("%s = %s, want %s", key, wire.Policy[key], want)
				}
			}
		})
	}
}

func TestAuthorizationAuditContainsMetadataOnly(t *testing.T) {
	r, _ := openTest(t, nil)
	origin := createTest(t, r, "omp", "origin")
	dest, err := r.Create(admin, model.CreateAgent{Harness: "claude", Name: "recipient", Cwd: t.TempDir(), Policy: model.Policy{Senders: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	payload := "password=never-store-this-message"
	_, err = r.Send(model.Principal{AgentID: origin.ID}, model.SendRequest{To: dest.ID, Text: payload})
	requireStatus(t, err, 403)
	var actor, recipient, operation, category string
	if err = r.db.QueryRow("SELECT actor,recipient,operation,category FROM audit").Scan(&actor, &recipient, &operation, &category); err != nil {
		t.Fatal(err)
	}
	if actor != origin.ID || recipient != dest.ID || operation != "send" || category != "forbidden" {
		t.Fatalf("unexpected denial metadata: %q %q %q %q", actor, recipient, operation, category)
	}
	if strings.Contains(logs.String(), payload) || strings.Contains(logs.String(), "never-store-this-message") {
		t.Fatal("denied message leaked to structured log")
	}
	// Untrusted routing fields are not safe audit metadata until normalized.
	blocked, err := r.Create(admin, model.CreateAgent{Harness: "omp", Name: "blocked", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	malformedTarget := "password=malformed-target-secret"
	_, err = r.Send(model.Principal{AgentID: blocked.ID}, model.SendRequest{To: malformedTarget, Text: payload})
	requireStatus(t, err, 403)
	if err = r.db.QueryRow("SELECT recipient FROM audit ORDER BY id DESC LIMIT 1").Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != "unknown" || strings.Contains(logs.String(), "malformed-target-secret") {
		t.Fatal("malformed target leaked to audit metadata")
	}
	credentialTarget := "omp:sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	_, err = r.Send(model.Principal{AgentID: blocked.ID}, model.SendRequest{To: credentialTarget, Text: payload})
	requireStatus(t, err, 403)
	if err = r.db.QueryRow("SELECT recipient FROM audit ORDER BY id DESC LIMIT 1").Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != "unknown" || strings.Contains(logs.String(), credentialTarget) {
		t.Fatal("credential-shaped canonical target leaked to audit metadata")
	}
}

func TestRecipientPolicyDoesNotGrantOutboundPermission(t *testing.T) {
	r, _ := openTest(t, nil)
	origin, err := r.Create(admin, model.CreateAgent{Harness: "omp", Name: "origin", Cwd: t.TempDir(), Policy: model.Policy{Operations: []string{"send"}}})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := r.Create(admin, model.CreateAgent{Harness: "claude", Name: "recipient", Cwd: t.TempDir(), Policy: model.Policy{Senders: []string{"*"}, InboundOperations: []string{"*"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Send(model.Principal{AgentID: origin.ID}, model.SendRequest{To: dest.ID, Text: "not permitted outbound"})
	requireStatus(t, err, 403)
}

func TestRedactRecognizedCredentials(t *testing.T) {
	for _, tc := range []struct{ name, input, secret string }{
		{"OpenAI project", "key sk-proj-abcdefghijklmnopqrstuvwxyz0123456789", "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"Anthropic", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"GitHub", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"GitHub fine grained", "github_pat_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"Slack", "xoxb" + "-123456789012-123456789012-abcdefABCDEF123456789", "xoxb" + "-123456789012-123456789012-abcdefABCDEF123456789"},
		{"Google", "AIzaSyabcdefghijklmnopqrstuvwxyz0123456789", "AIzaSyabcdefghijklmnopqrstuvwxyz0123456789"},
		{"AWS", "AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"GitLab", "glpat-abcdefghijklmnopqrstuvwxyz0123456789", "glpat-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"Hugging Face", "hf_abcdefghijklmnopqrstuvwxyz0123456789", "hf_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"Stripe", "sk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789", "sk_" + "live_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"SendGrid", "SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz0123456789", "SG.abcdefghijklmnopqrstuv.abcdefghijklmnopqrstuvwxyz0123456789"},
		{"npm", "npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"bearer", "Authorization: Bearer abc.DEF_123-456==", "abc.DEF_123-456=="},
		{"JSON bearer", `{"Authorization":"Bearer abc.DEF_123-456=="}`, "abc.DEF_123-456=="},
		{"environment", "OPENAI_API_KEY=opaque-provider-value", "opaque-provider-value"},
		{"GitHub environment", "GITHUB_TOKEN=opaque-github-value", "opaque-github-value"},
		{"assignment", "password='has spaces and punctuation!'", "has spaces and punctuation!"},
		{"JSON", `{"apiKey":"opaque-json-value","sessionId":"ses_unchanged"}`, "opaque-json-value"},
		{"escaped JSON", `{"password":"escaped\\\"secret"}`, `escaped\\\"secret`},
		{"URL", "https://user:p%40ssword@example.com/private", "user:p%40ssword"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.input)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("credential not redacted for %s", tc.name)
			}
			if redact(got) != got {
				t.Fatalf("redaction is not idempotent for %s", tc.name)
			}
			if strings.HasPrefix(tc.input, "{") && !json.Valid([]byte(got)) {
				t.Fatalf("redaction corrupted JSON for %s", tc.name)
			}
		})
	}
}

func TestRedactPreservesNativeSessionIdentifiers(t *testing.T) {
	for _, input := range []string{
		`{"sessionId":"ses_01JABCDEF1234567890","thread_id":"0198bbcc-aabb-7000-8000-123456789abc"}`,
		"native-abcdefghijklmnopqrstuvwxyz0123456789",
		"session_id=0198bbcc-aabb-7000-8000-123456789abc",
		"correlationId=abcdefghijklmnopqrstuvwxyz0123456789",
		"anthropic/claude-sonnet-4-5 openai/gpt-5",
	} {
		if got := redact(input); got != input {
			t.Fatalf("non-secret session or model identifier changed: %q", input)
		}
	}
}

func TestCreateRejectsUnsafePolicyAndRegistryValues(t *testing.T) {
	r, _ := openTest(t, nil)
	for _, tc := range []struct {
		name   string
		change func(*model.CreateAgent)
	}{
		{"sender ID", func(c *model.CreateAgent) { c.Policy.Senders = []string{"claude:Upper"} }},
		{"sender harness", func(c *model.CreateAgent) { c.Policy.Senders = []string{"unknown:agent"} }},
		{"inbound operation", func(c *model.CreateAgent) { c.Policy.InboundOperations = []string{"start"} }},
		{"writes", func(c *model.CreateAgent) { c.Policy.WorktreeWrites = true }},
		{"display control", func(c *model.CreateAgent) { c.DisplayName = "name\x1b[31m" }},
		{"display credential", func(c *model.CreateAgent) { c.DisplayName = "ghp_abcdefghijklmnopqrstuvwxyz0123456789" }},
		{"provider control", func(c *model.CreateAgent) { c.Config.Provider = "native\tprovider" }},
		{"model credential", func(c *model.CreateAgent) { c.Config.Model = "AIzaSyabcdefghijklmnopqrstuvwxyz0123456789" }},
		{"path control", func(c *model.CreateAgent) { c.Cwd += "\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := model.CreateAgent{Harness: "omp", Name: "unsafe", Cwd: t.TempDir()}
			tc.change(&c)
			_, err := r.Create(admin, c)
			requireStatus(t, err, 400)
		})
	}
}

func TestStartRevalidatesCanonicalWorkspace(t *testing.T) {
	r, _ := openTest(t, nil)
	called := false
	r.adapters["omp"] = model.Adapter{Factory: func(context.Context, model.Agent, model.Emit) (model.Runtime, error) {
		called = true
		return &fakeRuntime{session: "native-workspace"}, nil
	}}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := r.Create(admin, model.CreateAgent{Harness: "omp", Name: "workspace", Cwd: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(workspace, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}
	_, err = r.Action(admin, a.ID, "start")
	requireStatus(t, err, 400)
	if called {
		t.Fatal("runtime factory invoked after canonical directory was replaced")
	}
}

func TestSplitStreamCredentialsNeverReachEvents(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	var turnToken string
	r, _ := openTest(t, func(_ context.Context, turn model.Turn, emit model.Emit) (string, error) {
		turnToken = turn.Token
		emit("delta", "sk-proj-")
		emit("delta", "abcdefghijklmnopqrstuvwxyz0123456789")
		emit("delta", "\n")
		emit("delta", turn.Token[:32])
		emit("delta", turn.Token[32:])
		return "safe final reply", nil
	})
	origin := createTest(t, r, "omp", "origin")
	dest := createTest(t, r, "codex", "dest")
	startTest(t, r, dest.ID)
	task := sendTest(t, r, origin.ID, dest.ID, "never persist streamed credentials")
	if got := awaitTask(t, r, task.ID); got.State != "completed" {
		t.Fatal(got)
	}
	events, err := r.Events(admin, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, event := range events {
		if event.Type == "delta" {
			text.WriteString(event.Data)
		}
	}
	if strings.Contains(text.String(), secret) || strings.Contains(text.String(), turnToken) ||
		strings.Contains(text.String(), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("split native credentials survived durable text events")
	}
	if !strings.Contains(text.String(), "[REDACTED]") {
		t.Fatal("sanitized output was not exposed")
	}
}
