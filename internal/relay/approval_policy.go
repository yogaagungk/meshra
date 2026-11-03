package relay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"meshra/internal/model"
)

type approvalRule struct {
	ID          string `json:"id"`
	ProfileID   string `json:"profileId"`
	Tool        string `json:"tool"`
	MatcherKind string `json:"matcherKind"`
	Matcher     string `json:"matcher"`
	CreatedBy   string `json:"createdBy"`
	CreatedAt   string `json:"createdAt"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Revision    int    `json:"revision"`
}

func validRulePath(s string, glob bool) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") || strings.ContainsRune(s, 0) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	if glob {
		_, err := path.Match(s, "example")
		return err == nil
	}
	return !strings.ContainsAny(s, "*?[")
}

func validateApprovalRule(rule approvalRule) error {
	if rule.ProfileID == "" || len(rule.Matcher) > 8192 {
		return model.Err(400, "invalid approval rule")
	}
	switch rule.MatcherKind {
	case "argv_exact", "argv_prefix":
		if rule.Tool != "meshra_workspace_command" {
			return model.Err(400, "argv rules require workspace command")
		}
		var argv []string
		if json.Unmarshal([]byte(rule.Matcher), &argv) != nil || len(argv) == 0 || len(argv) > 128 {
			return model.Err(400, "matcher must be a nonempty JSON argv array")
		}
		for _, arg := range argv {
			if arg == "" || strings.ContainsRune(arg, 0) {
				return model.Err(400, "invalid argv matcher")
			}
		}
	case "path_glob":
		if rule.Tool != "meshra_workspace_write" || !validRulePath(rule.Matcher, true) {
			return model.Err(400, "invalid workspace path glob")
		}
	default:
		return model.Err(400, "invalid matcher kind")
	}
	if rule.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339Nano, rule.ExpiresAt)
		if err != nil || !t.After(now()) {
			return model.Err(400, "rule expiry must be in the future")
		}
		rule.ExpiresAt = t.UTC().Format(time.RFC3339Nano)
	}
	return nil
}

func approvalRuleMatches(rule approvalRule, action *toolAction) bool {
	switch rule.MatcherKind {
	case "argv_exact", "argv_prefix":
		q, ok := action.args.(*preparedCommand)
		if !ok || q.input.Shell != "" {
			return false
		}
		var expected []string
		if json.Unmarshal([]byte(rule.Matcher), &expected) != nil || len(expected) == 0 {
			return false
		}
		actual := q.input.Argv
		if len(actual) < len(expected) || (rule.MatcherKind == "argv_exact" && len(actual) != len(expected)) {
			return false
		}
		for i := range expected {
			if expected[i] != actual[i] {
				return false
			}
		}
		return true
	case "path_glob":
		q, ok := action.args.(*toolWriteArgs)
		if !ok || !validRulePath(q.Path, false) || !validRulePath(rule.Matcher, true) {
			return false
		}
		matched, err := path.Match(rule.Matcher, q.Path)
		return err == nil && matched
	}
	return false
}

// Called with the relay mutex held, after the immutable action and hash are prepared.
func (b *toolBroker) matchingApprovalRuleLocked(action *toolAction, name string) (string, error) {
	if b.snapshot.Binding.Harness != "openai" || b.snapshot.ConsoleSessionOwner == "" || b.snapshot.External || (name != "meshra_workspace_command" && name != "meshra_workspace_write") {
		return "", nil
	}
	var policy string
	if err := b.relay.db.QueryRow("SELECT approval_policy FROM profile_tool_policies WHERE profile_id=?", b.snapshot.Profile.ID).Scan(&policy); err != nil {
		return "", err
	}
	if policy != "allowlist" {
		return "", nil
	}
	rows, err := b.relay.db.Query("SELECT id,matcher_kind,matcher,coalesce(expires_at,'') FROM tool_allow_rules WHERE profile_id=? AND tool=? AND revoked_at IS NULL ORDER BY created_at,id", b.snapshot.Profile.ID, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var rule approvalRule
		if err = rows.Scan(&rule.ID, &rule.MatcherKind, &rule.Matcher, &rule.ExpiresAt); err != nil {
			return "", err
		}
		if rule.ExpiresAt != "" {
			deadline, e := time.Parse(time.RFC3339Nano, rule.ExpiresAt)
			if e != nil || !deadline.After(now()) {
				continue
			}
		}
		if approvalRuleMatches(rule, action) {
			return rule.ID, nil
		}
	}
	return "", rows.Err()
}

// Policy and rule updates invalidate admitted actions as well as their approvals.
func invalidateApprovalPolicyTx(tx *sql.Tx, profile string) error {
	if _, err := tx.Exec("UPDATE profile_tool_policies SET revision=revision+1 WHERE profile_id=?", profile); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE tool_approvals SET status='invalidated',revision=revision+1 WHERE status IN ('pending','approved','answered') AND invocation_id IN (SELECT id FROM tool_invocations WHERE json_extract(body,'$.profileId')=?)", profile); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE tool_invocations SET state='cancelled' WHERE state IN ('pending','approved','auto_approved','waiting_question') AND json_extract(body,'$.profileId')=?", profile)
	return err
}
func (r *Relay) signalPolicyCallsLocked(profile string) {
	for _, b := range r.toolBrokers {
		if b.snapshot.Profile.ID == profile {
			for _, call := range b.calls {
				signalToolCall(call)
			}
		}
	}
}

func (r *Relay) approvalPolicyOperation(ctx context.Context, p model.Principal, op string, raw json.RawMessage) (any, error) {
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var q struct {
		ProfileID   string `json:"profileId"`
		Policy      string `json:"policy"`
		ID          string `json:"id"`
		Tool        string `json:"tool"`
		MatcherKind string `json:"matcherKind"`
		Matcher     string `json:"matcher"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if err := decodeAutomation(raw, &q); err != nil {
		return nil, err
	}
	if q.ProfileID == "" {
		return nil, model.Err(400, "profileId required")
	}
	if err := r.requireConsole(p); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var policy string
	if err := r.db.QueryRow("SELECT approval_policy FROM profile_tool_policies WHERE profile_id=?", q.ProfileID).Scan(&policy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, model.Err(404, "profile not found")
		}
		return nil, err
	}
	switch op {
	case "approval_policy.get":
		return map[string]any{"profileId": q.ProfileID, "policy": policy}, nil
	case "approval_policy.set":
		if q.Policy != "manual" && q.Policy != "allowlist" {
			return nil, model.Err(400, "policy must be manual or allowlist")
		}
		if q.Policy == policy {
			return map[string]any{"profileId": q.ProfileID, "policy": policy}, nil
		}
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE profile_tool_policies SET approval_policy=? WHERE profile_id=?", q.Policy, q.ProfileID); err == nil {
			err = invalidateApprovalPolicyTx(tx, q.ProfileID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			return nil, err
		}
		r.signalPolicyCallsLocked(q.ProfileID)
		return map[string]any{"profileId": q.ProfileID, "policy": q.Policy}, nil
	case "approval_rules.list":
		rows, err := r.db.Query("SELECT id,profile_id,tool,matcher_kind,matcher,created_by,created_at,coalesce(expires_at,''),revision FROM tool_allow_rules WHERE profile_id=? AND revoked_at IS NULL ORDER BY created_at,id LIMIT 500", q.ProfileID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []approvalRule{}
		for rows.Next() {
			var v approvalRule
			if err = rows.Scan(&v.ID, &v.ProfileID, &v.Tool, &v.MatcherKind, &v.Matcher, &v.CreatedBy, &v.CreatedAt, &v.ExpiresAt, &v.Revision); err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return map[string]any{"items": items}, rows.Err()
	case "approval_rules.create":
		rule := approvalRule{ID: randomID(), ProfileID: q.ProfileID, Tool: q.Tool, MatcherKind: q.MatcherKind, Matcher: q.Matcher, CreatedBy: p.ConsoleSessionID, CreatedAt: now().UTC().Format(time.RFC3339Nano), ExpiresAt: q.ExpiresAt, Revision: 1}
		if err := validateApprovalRule(rule); err != nil {
			return nil, err
		}
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		_, err = tx.Exec("INSERT INTO tool_allow_rules(id,profile_id,tool,matcher_kind,matcher,created_by,created_at,expires_at,revision) VALUES(?,?,?,?,?,?,?,NULLIF(?,''),1)", rule.ID, rule.ProfileID, rule.Tool, rule.MatcherKind, rule.Matcher, rule.CreatedBy, rule.CreatedAt, rule.ExpiresAt)
		if err == nil {
			err = invalidateApprovalPolicyTx(tx, q.ProfileID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			return nil, err
		}
		r.signalPolicyCallsLocked(q.ProfileID)
		return rule, nil
	case "approval_rules.revoke":
		tx, err := r.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		res, err := tx.Exec("UPDATE tool_allow_rules SET revoked_at=?,revision=revision+1 WHERE id=? AND profile_id=? AND revoked_at IS NULL", now().UTC().Format(time.RFC3339Nano), q.ID, q.ProfileID)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, model.Err(404, "rule not found")
		}
		if err = invalidateApprovalPolicyTx(tx, q.ProfileID); err == nil {
			err = tx.Commit()
		}
		if err != nil {
			return nil, err
		}
		r.signalPolicyCallsLocked(q.ProfileID)
		return map[string]any{"id": q.ID}, nil
	case "approval_suggestions.list":
		rows, err := r.db.Query(`SELECT json_extract(i.body,'$.name'),coalesce(json_extract(i.body,'$.preview.arguments'),'null'),count(*) FROM tool_invocations i JOIN tool_approvals a ON a.invocation_id=i.id WHERE json_extract(i.body,'$.profileId')=? AND json_extract(a.body,'$.decision')='approve' AND json_extract(i.body,'$.name') IN ('meshra_workspace_command','meshra_workspace_write') AND coalesce(json_extract(i.body,'$.argumentsHash'),'')<>'' GROUP BY json_extract(i.body,'$.name'),json_extract(i.body,'$.argumentsHash') HAVING count(*)>=2 ORDER BY count(*) DESC LIMIT 100`, q.ProfileID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var name, args string
			var count int
			if err = rows.Scan(&name, &args, &count); err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"tool": name, "arguments": json.RawMessage(args), "count": count})
		}
		return map[string]any{"items": items}, rows.Err()
	}
	return nil, model.Err(404, "unknown approval policy operation")
}
