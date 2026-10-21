package omp

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
	"testing"
	"time"
)

func TestQuotaNormalizationAndPrivacy(t *testing.T) {
	raw := []byte(`{"reports":[{"provider":"anthropic","fetchedAt":1792424700000,"metadata":{"email":"private@example.test"},"limits":[{"id":"secret","label":"Secret account","window":{"label":"5 Hour","resetsAt":1792434700000},"amount":{"usedFraction":0.7,"unit":"percent"}}]}]}`)
	items, err := parseQuotas(raw)
	if err != nil || len(items) != 1 || items[0].UsedPercent == nil || *items[0].UsedPercent != 70 || items[0].ResetsAt == nil {
		t.Fatal(items, err)
	}
	if items[0].Label != "5 Hour" || items[0].Account != "Account 1" {
		t.Fatal(items)
	}
}
func TestNativeContextEstimate(t *testing.T) {
	mockOMP(t, "context")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New().Factory(ctx, testAgent(t, "context"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var value model.ContextObservation
	_, err = rt.Run(ctx, model.Turn{}, func(kind, data string) {
		if kind == "context_usage" {
			json.Unmarshal([]byte(data), &value)
		}
	})
	if err != nil || value.UsedTokens != 160 || value.WindowTokens != 1000 || !value.Estimated {
		t.Fatal(value, err)
	}
}
