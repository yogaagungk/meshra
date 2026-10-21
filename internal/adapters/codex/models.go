package codex

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
)

func (r *runtime) Models(ctx context.Context) ([]model.ModelOption, error) {
	r.run.Lock()
	defer r.run.Unlock()
	data, err := r.command(ctx, "model/list", map[string]any{"limit": 100}, nil)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Data []struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Name   string `json:"displayName"`
			Hidden bool   `json:"hidden"`
		} `json:"data"`
	}
	if err = json.Unmarshal(data, &reply); err != nil {
		return nil, err
	}
	out := []model.ModelOption{}
	for _, v := range reply.Data {
		if v.Hidden {
			continue
		}
		id := v.Model
		if id == "" {
			id = v.ID
		}
		out = append(out, model.ModelOption{ID: id, Name: v.Name})
	}
	return out, nil
}
