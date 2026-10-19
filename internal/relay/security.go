package relay

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"meshra/internal/model"
)

// Recipient policies supplement, rather than replace, the sender's outbound
// policy. Nil lists preserve v0.1; allocated empty lists deliberately deny all.
// The caller holds r.mu, including while recording a denial.
func (r *Relay) authorizeRecipient(p model.Principal, from string, dest model.Agent, op string) error {
	if p.Admin {
		return nil
	}
	if dest.Policy.Senders != nil && !contains(dest.Policy.Senders, from) {
		r.auditDenied(p, dest.ID, op)
		return model.Err(403, "sender not permitted by recipient")
	}
	if dest.Policy.InboundOperations != nil && !contains(dest.Policy.InboundOperations, op) {
		r.auditDenied(p, dest.ID, op)
		return model.Err(403, "operation not permitted by recipient")
	}
	return nil
}

func validAgentID(id string) bool {
	parts := strings.Split(id, ":")
	return len(parts) == 2 && namePattern.MatchString(parts[1]) &&
		(parts[0] == "omp" || parts[0] == "claude" || parts[0] == "codex" || parts[0] == "opencode")
}

func validOperation(op string) bool {
	switch op {
	case "send", "request", "status", "events", "cancel", "list":
		return true
	}
	return false
}

// auditDenied never accepts message content or an error string. Even malformed
// untrusted identity/operation parameters are normalized before storage/logging.
func (r *Relay) auditDenied(p model.Principal, recipient, operation string) {
	actor := p.AgentID
	if p.Admin {
		actor = "admin"
	} else if !validAgentID(actor) || redact(actor) != actor {
		actor = "unknown"
	}
	if !validAgentID(recipient) || redact(recipient) != recipient {
		recipient = "unknown"
	}
	if !validOperation(operation) {
		operation = "unknown"
	}
	_, err := r.db.Exec("INSERT INTO audit(time,actor,recipient,operation,category) VALUES(?,?,?,?,?)", now().Format(time.RFC3339Nano), actor, recipient, operation, "forbidden")
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("authorization denied", "actor", actor, "recipient", recipient, "operation", operation, "category", "forbidden")
	if err != nil {
		// SQLite errors can contain values; report only the fact of failure.
		logger.Error("authorization audit persistence failed", "category", "audit_storage")
	}
}

func safeRegistryText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, ch := range s {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func canonicalWorkspace(path string) (string, error) {
	if path == "" || !safeRegistryText(path) {
		return "", model.Err(400, "working directory must be a nonempty path without control characters")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", model.Err(400, "working directory is invalid")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", model.Err(400, "working directory does not exist")
	}
	st, err := os.Stat(canonical)
	if err != nil || !st.IsDir() {
		return "", model.Err(400, "working directory is not a directory")
	}
	return canonical, nil
}

// Registration stores an absolute canonical directory. Re-resolve it before
// Factory so a removed directory or replacement symlink cannot silently move a
// registered identity into a different workspace. This is not an OS sandbox and
// cannot eliminate filesystem races after the check.
func validateWorkspace(a model.Agent) error {
	canonical, err := canonicalWorkspace(a.Cwd)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(a.Cwd) || canonical != a.Cwd {
		return model.Err(400, "registered working directory is no longer canonical")
	}
	return nil
}

var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{12,}`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\b(?:glpat-|hf_)[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}`),
}

const secretField = `\b(?:[a-z0-9]+[_-])*(?:api[_-]?key|(?:api|access|refresh|auth|bearer)[_-]?token|(?:client[_-]?)?secret|(?:private|secret)[_-]?key|password|passwd|token|key)`

var (
	bearerCredential   = regexp.MustCompile(`(?i)(\bbearer[ \t]+)[A-Za-z0-9._~+/-]+=*`)
	urlCredential      = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/@]+@`)
	doubleQuotedSecret = regexp.MustCompile(`(?i)(["']?` + secretField + `["']?\s*[:=]\s*")((?:\\.|[^"\\])*)(")`)
	singleQuotedSecret = regexp.MustCompile(`(?i)(["']?` + secretField + `["']?\s*[:=]\s*')((?:\\.|[^'\\])*)(')`)
	unquotedSecret     = regexp.MustCompile(`(?i)(["']?` + secretField + `["']?\s*[:=]\s*)(\x5bREDACTED\x5d|[^\s,;\}\]"']+)`)
)

// redact recognizes common credential formats and labelled secrets. It is
// deliberately not an entropy filter: native session IDs, correlation IDs and
// ordinary source text must not be blindly removed. Arbitrary secrets can still
// be present in free text, so access control and filesystem protection remain
// necessary. Callers must hash original requests before applying this function.
func redact(s string) string {
	s = doubleQuotedSecret.ReplaceAllString(s, "${1}[REDACTED]${3}")
	s = singleQuotedSecret.ReplaceAllString(s, "${1}[REDACTED]${3}")
	s = unquotedSecret.ReplaceAllString(s, "${1}[REDACTED]")
	s = bearerCredential.ReplaceAllString(s, "${1}[REDACTED]")
	s = urlCredential.ReplaceAllString(s, "${1}[REDACTED]@")
	for _, pattern := range credentialPatterns {
		s = pattern.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
