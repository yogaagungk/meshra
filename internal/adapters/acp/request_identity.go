package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

type clientRequestIDKey struct{}

func canonicalRequestID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || len(raw) > 512 {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return "s:" + text, text != "" && len(text) <= 256
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil {
		return "", false
	}
	value, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil {
		return "", false
	}
	return "n:" + strconv.FormatInt(value, 10), true
}

// Only the transport installs this key. Keeping one identity across retries
// lets the broker reject changed arguments and avoid replaying claimed effects.
func requestCallID(ctx context.Context) (string, *rpcError) {
	id, ok := ctx.Value(clientRequestIDKey{}).(string)
	if !ok || id == "" {
		return "", &rpcError{Code: -32603, Message: "trusted ACP request identity is unavailable"}
	}
	hash := sha256.Sum256([]byte(id))
	return "acp-rpc:" + hex.EncodeToString(hash[:]), nil
}
