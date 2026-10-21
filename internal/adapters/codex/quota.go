package codex

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
	"sort"
	"strconv"
	"time"
)

func (r *runtime) Quotas(ctx context.Context) ([]model.QuotaWindow, error) {
	r.run.Lock()
	defer r.run.Unlock()
	raw, err := r.command(ctx, "account/rateLimits/read", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseQuotas(raw)
}
func parseQuotas(raw []byte) ([]model.QuotaWindow, error) {
	type window struct {
		Used     *float64 `json:"usedPercent"`
		Duration int      `json:"windowDurationMins"`
		Reset    *int64   `json:"resetsAt"`
	}
	type bucket struct {
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var response struct {
		Default bucket            `json:"rateLimits"`
		Buckets map[string]bucket `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	keys := []string{}
	for id := range response.Buckets {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		keys = []string{"codex"}
		response.Buckets = map[string]bucket{"codex": response.Default}
	}
	out := []model.QuotaWindow{}
	seenAt := time.Now().UTC()
	for _, id := range keys {
		b := response.Buckets[id]
		for i, w := range []*window{b.Primary, b.Secondary} {
			if w == nil {
				continue
			}
			label := "Primary window"
			if i == 1 {
				label = "Secondary window"
			}
			if w.Duration > 0 && w.Duration <= 525600 {
				suffix := strconv.Itoa(w.Duration) + " minute"
				if w.Duration%1440 == 0 {
					suffix = strconv.Itoa(w.Duration/1440) + " day"
				} else if w.Duration%60 == 0 {
					suffix = strconv.Itoa(w.Duration/60) + " hour"
				}
				label += " " + suffix
			}
			var reset *time.Time
			if w.Reset != nil && *w.Reset > 0 {
				v := time.Unix(*w.Reset, 0).UTC()
				reset = &v
			}
			out = append(out, model.QuotaWindow{ID: id + "/" + label, Provider: "codex", Label: id + " " + label, UsedPercent: w.Used, ResetsAt: reset, ObservedAt: &seenAt})
		}
	}
	return out, nil
}
