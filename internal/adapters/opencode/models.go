package opencode

import (
	"context"
	"meshra/internal/model"
	"sort"
)

func (r *runtimeSession) Models(ctx context.Context) ([]model.ModelOption, error) {
	s, err := r.start(ctx, r.agent, model.Turn{})
	if err != nil {
		return nil, err
	}
	defer s.stop()
	var reply struct {
		Providers []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err = s.call(ctx, "GET", "/config/providers", nil, &reply); err != nil {
		return nil, err
	}
	out := []model.ModelOption{}
	for _, p := range reply.Providers {
		for id, m := range p.Models {
			out = append(out, model.ModelOption{ID: id, Name: m.Name, Provider: p.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider+out[i].ID < out[j].Provider+out[j].ID })
	return out, nil
}
