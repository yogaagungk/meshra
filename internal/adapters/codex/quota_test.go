package codex

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
	"testing"
	"time"
)

func TestQuotaNativeProtocol(t *testing.T) {
	mockCodex(t, "success")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "quota"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	items, err := rt.(model.QuotaReader).Quotas(ctx)
	if err != nil || len(items) != 1 || items[0].UsedPercent == nil || *items[0].UsedPercent != 25 || items[0].ResetsAt == nil {
		t.Fatal(items, err)
	}
}
func TestNativeContextObservation(t *testing.T) {
	mockCodex(t, "usage")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "context"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var contextValue model.ContextObservation
	var cached int64
	_, err = rt.Run(ctx, model.Turn{}, func(kind, data string) {
		if kind == "context_usage" {
			json.Unmarshal([]byte(data), &contextValue)
		}
		if kind == "usage" {
			var v struct {
				Cached int64 `json:"cachedInputTokens"`
			}
			json.Unmarshal([]byte(data), &v)
			cached = v.Cached
		}
	})
	if err != nil || contextValue.UsedTokens != 160 || contextValue.WindowTokens != 1000 || contextValue.Estimated || cached != 80 {
		t.Fatal(contextValue, cached, err)
	}
}
