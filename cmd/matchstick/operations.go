package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fakta-ai/matchstick/internal/config"
	"github.com/fakta-ai/matchstick/internal/metrics"
	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/pool"
)

// DecideInput is the decide operation's input.
type DecideInput struct {
	Observation plan.Observation `json:"observation" jsonschema:"what one round of a pool observed"`
	Limits      plan.Limits      `json:"limits" jsonschema:"the pool's limits"`
}

// PoolsInput selects the configuration and, optionally, one pool.
type PoolsInput struct {
	Config string `json:"config,omitempty" jsonschema:"the configuration file (default matchstick.yaml)"`
	Pool   string `json:"pool,omitempty" jsonschema:"only this pool"`
}

// PlanOutput is the plan operation's output.
type PlanOutput struct {
	Pools []PoolPlan `json:"pools"`
}

// PoolPlan is what one pool's round would do now.
type PoolPlan struct {
	Pool        string           `json:"pool"`
	Observation plan.Observation `json:"observation"`
	Decision    plan.Decision    `json:"decision"`
}

// StatusOutput is the status operation's output.
type StatusOutput struct {
	Pools []PoolStatus `json:"pools"`
}

// PoolStatus is one pool as the cloud and GitHub report it.
type PoolStatus struct {
	Pool     string         `json:"pool"`
	Demand   *int           `json:"demand" jsonschema:"jobs assigned to the pool, waiting or running; null if unknown"`
	Machines []plan.Machine `json:"machines"`
	// Runners is null when GitHub's runner list could not be read.
	Runners []plan.Runner `json:"runners" jsonschema:"the pool's registrations; null if GitHub could not be read"`
}

func operations() []*op.Operation {
	return []*op.Operation{
		op.Define("decide",
			"Decide what a pool should do, from an observation and the pool's limits. Pure: reads nothing, changes nothing.",
			"matchstick decide --input observation.json --json",
			op.Annotations{ReadOnly: true, Idempotent: true},
			func(_ context.Context, in DecideInput) (plan.Decision, error) {
				return plan.Decide(in.Observation, in.Limits), nil
			}),
		op.Define("plan",
			"Gather each pool now and show what a round would delete and create. Changes nothing.",
			"matchstick plan --config matchstick.yaml --json",
			op.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
			planPools),
		op.Define("status",
			"Show each pool's machines, runner registrations and demand, as the cloud and GitHub report them.",
			"matchstick status --config matchstick.yaml --pool build",
			op.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
			statusPools),
	}
}

// observe builds the selected pools from the configuration and gathers each once.
func observe(ctx context.Context, in PoolsInput, each func(config.Pool, plan.Observation)) error {
	cfg, err := loadConfig(in.Config)
	if err != nil {
		return err
	}
	pools, err := selectPools(cfg, in.Pool)
	if err != nil {
		return err
	}
	b, err := newBackends(cfg, pools)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New(prometheus.NewRegistry())
	for _, pc := range pools {
		be := b[pc.Name]
		p := pool.New(poolSettings(cfg, pc), be.machines, be.runners, m, log, now)
		demand, err := be.demand(ctx)
		if err != nil {
			return op.Errorf(op.CodeUnavailable, "pool %s: demand: %v", pc.Name, err)
		}
		if demand != nil {
			p.SetDemand(*demand)
		}
		o, err := p.Observe(ctx)
		if err != nil {
			return op.Errorf(op.CodeUnavailable, "pool %s: %v", pc.Name, err)
		}
		each(pc, o)
	}
	return nil
}

func planPools(ctx context.Context, in PoolsInput) (PlanOutput, error) {
	out := PlanOutput{Pools: []PoolPlan{}}
	err := observe(ctx, in, func(pc config.Pool, o plan.Observation) {
		out.Pools = append(out.Pools, PoolPlan{Pool: pc.Name, Observation: o, Decision: plan.Decide(o, pc.Limits())})
	})
	return out, err
}

func statusPools(ctx context.Context, in PoolsInput) (StatusOutput, error) {
	out := StatusOutput{Pools: []PoolStatus{}}
	err := observe(ctx, in, func(pc config.Pool, o plan.Observation) {
		out.Pools = append(out.Pools, PoolStatus{Pool: pc.Name, Demand: o.Demand, Machines: o.Machines,
			Runners: o.Runners})
	})
	return out, err
}

const defaultConfig = "matchstick.yaml"

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = defaultConfig
	}
	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, op.Errorf(op.CodeInvalidConfig, "no configuration at %s; pass --config <file>", path)
	}
	if err != nil {
		return nil, op.Errorf(op.CodeInvalidConfig, "%v", err)
	}
	return cfg, nil
}

func selectPools(cfg *config.Config, name string) ([]config.Pool, error) {
	if name == "" {
		return cfg.Pools, nil
	}
	for _, p := range cfg.Pools {
		if p.Name == name {
			return []config.Pool{p}, nil
		}
	}
	return nil, op.Errorf(op.CodeInvalidInput, "no pool %q in the configuration", name)
}

func poolSettings(cfg *config.Config, p config.Pool) pool.Settings {
	return pool.Settings{Instance: cfg.Instance, Name: p.Name, Limits: p.Limits(),
		MaxLifetime: time.Duration(p.MaxLifetime), Interval: time.Duration(cfg.Interval)}
}

// now is the clock, in UTC.
func now() time.Time { return time.Now().UTC() }
