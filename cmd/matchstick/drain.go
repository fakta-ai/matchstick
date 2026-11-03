package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/server"
)

func drainPool(ctx context.Context, in DrainInput) (DrainOutput, error) {
	cfg, err := loadConfig(in.Config)
	if err != nil {
		return DrainOutput{}, err
	}
	if _, err := selectPools(cfg, in.Pool); err != nil {
		return DrainOutput{}, err
	}
	body, err := json.Marshal(server.DrainRequest{Draining: !in.Stop})
	if err != nil {
		return DrainOutput{}, err
	}
	url := fmt.Sprintf("http://%s/pools/%s/drain", cfg.Listen.Admin, in.Pool)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return DrainOutput{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return DrainOutput{}, op.Errorf(op.CodeUnavailable,
			"no running matchstick at %s (listen.admin): %v", cfg.Listen.Admin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return DrainOutput{}, op.Errorf(op.CodeUnavailable, "drain: %s", resp.Status)
	}
	var state server.PoolState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return DrainOutput{}, op.Errorf(op.CodeUnavailable, "drain: %v", err)
	}
	return DrainOutput{Pool: state.Pool, Draining: state.Draining}, nil
}
