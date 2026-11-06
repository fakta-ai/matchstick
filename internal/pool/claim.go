package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fakta-ai/matchstick/internal/github"
	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/provider"
)

// ErrRefused marks a claim that will never succeed: the pool holds no configuration for the machine, or the proof does
// not show the claimant is that machine.
var ErrRefused = errors.New("claim refused")

// Bootstrap is what a machine is given at creation: public information only, everything it needs to claim its runner
// configuration.
type Bootstrap struct {
	// Machine is the machine's name.
	Machine string `json:"machine"`
	// ClaimURL is the endpoint the machine posts its claim to.
	ClaimURL string `json:"claimURL"`
	// ClaimCertificate is the endpoint's certificate, PEM: the only one the machine trusts for ClaimURL.
	ClaimCertificate string `json:"claimCertificate"`
}

// held is a runner configuration waiting for its machine's claim.
type held struct {
	config    string
	createdAt time.Time
}

func (p *Pool) bootstrap(name string) ([]byte, error) {
	b, err := json.Marshal(Bootstrap{Machine: name, ClaimURL: p.settings.Claim.URL,
		ClaimCertificate: p.settings.Claim.Certificate})
	if err != nil {
		return nil, fmt.Errorf("bootstrap of %s: %w", name, err)
	}
	return b, nil
}

func (p *Pool) hold(name string, jit github.JIT, createdAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held[name] = held{config: jit.Config, createdAt: createdAt}
}

func (p *Pool) drop(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, name)
}

// dropHeld drops the configurations of machines the decision deletes, and of machines older than the registration
// timeout, which the unregistered rule deletes anyway.
func (p *Pool) dropHeld(o plan.Observation, d plan.Decision) {
	timeout := time.Duration(p.settings.Limits.RegistrationTimeout)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, del := range d.Deletions {
		delete(p.held, del.Machine)
	}
	for name, h := range p.held {
		if o.Now.Sub(h.createdAt) > timeout {
			delete(p.held, name)
		}
	}
}

// waiting reports whether the pool holds a configuration for machine name that it may still release.
func (p *Pool) waiting(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.held[name]
	return ok && p.now().Sub(h.createdAt) <= time.Duration(p.settings.Limits.RegistrationTimeout)
}

// take removes machine name's configuration and returns it; false if another claim took it first or it was dropped.
func (p *Pool) take(name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.held[name]
	delete(p.held, name)
	return h.config, ok
}

// Owns reports whether name is one of this pool's machine names.
func (p *Pool) Owns(name string) bool {
	_, err := naming.Created(p.settings.Instance, p.settings.Name, name)
	return err == nil
}

// Claim releases machine name's runner configuration to the caller whose proof shows it is that machine, once: the
// configuration leaves the pool's memory as it is released. An error wrapping ErrRefused is final; any other means the
// proof could not be checked now, and the configuration is still held.
func (p *Pool) Claim(ctx context.Context, name string, proof json.RawMessage) (string, error) {
	log := p.log.With("machine", name)
	result := func(r string) { p.metrics.Claims.WithLabelValues(p.settings.Name, r).Inc() }
	if !p.Owns(name) || !p.waiting(name) {
		result("refused")
		log.Warn("claim refused", "why", "no configuration held for this machine")
		return "", fmt.Errorf("%w: no configuration held for %s", ErrRefused, name)
	}
	if err := p.machines.Identify(ctx, name, proof); err != nil {
		if errors.Is(err, provider.ErrNotProven) {
			result("refused")
			log.Warn("claim refused", "why", err.Error())
			return "", fmt.Errorf("%w: %w", ErrRefused, err)
		}
		result("failed")
		log.Warn("claim could not be checked", "error", err)
		return "", err
	}
	config, ok := p.take(name)
	if !ok {
		result("refused")
		log.Warn("claim refused", "why", "the configuration was released or dropped meanwhile")
		return "", fmt.Errorf("%w: no configuration held for %s", ErrRefused, name)
	}
	result("released")
	log.Info("runner configuration released")
	return config, nil
}
