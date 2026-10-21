package omp

import (
	"context"
	"encoding/json"
	"meshra/internal/model"
)

func (r *runtime) Models(ctx context.Context) ([]model.ModelOption, error) {
	r.run.Lock()
	defer r.run.Unlock()
	data, err := r.command(ctx, "get_available_models", nil)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Provider string `json:"provider"`
		} `json:"models"`
	}
	if err = json.Unmarshal(data, &reply); err != nil {
		return nil, err
	}
	out := []model.ModelOption{}
	for _, v := range reply.Models {
		out = append(out, model.ModelOption{ID: v.ID, Name: v.Name, Provider: v.Provider})
	}
	return out, nil
}
