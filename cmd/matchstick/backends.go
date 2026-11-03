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
	// demand reads the pool's demand once, for one-shot operations; nil if unknown.
	demand func(context.Context) (*int, error)
}

// backends builds each pool's backend from the configuration.
type backends interface {
	pool(config.Pool) backend
}

func newBackends(_ context.Context, cfg *config.Config) (backends, error) {
	if cfg.Fake != nil {
		return fakeBackends{cfg.Fake}, nil
	}
	return nil, op.Errorf(op.CodeInvalidConfig, "this build supports only the fake backend")
}

type fakeBackends struct{ fake *config.Fake }

func (f fakeBackends) pool(p config.Pool) backend {
	fp := f.fake.Pools[p.Name]
	return backend{
		machines: providerfake.New(now, fp.Machines...),
		runners:  githubfake.New(now, fp.Runners...),
		demand:   func(context.Context) (*int, error) { return fp.Demand, nil },
	}
}
