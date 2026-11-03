package main

import (
	"context"

	"github.com/fakta-ai/matchstick/internal/config"
	"github.com/fakta-ai/matchstick/internal/github"
	githubfake "github.com/fakta-ai/matchstick/internal/github/fake"
	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/provider"
	providerfake "github.com/fakta-ai/matchstick/internal/provider/fake"
)

// backend is one pool's cloud and GitHub.
type backend struct {
	machines provider.Provider
	runners  github.Runners
	// demand reads the pool's demand once; nil if unknown.
	demand func(context.Context) (*int, error)
}

// newBackends builds each pool's backend from the configuration. This build supports the fake only.
func newBackends(cfg *config.Config, pools []config.Pool) (map[string]backend, error) {
	if cfg.Fake == nil {
		return nil, op.Errorf(op.CodeInvalidConfig, "this build supports only the fake backend")
	}
	out := map[string]backend{}
	for _, p := range pools {
		fp := cfg.Fake.Pools[p.Name]
		out[p.Name] = backend{
			machines: providerfake.New(now, fp.Machines...),
			runners:  githubfake.New(now, fp.Runners...),
			demand:   func(context.Context) (*int, error) { return fp.Demand, nil },
		}
	}
	return out, nil
}
