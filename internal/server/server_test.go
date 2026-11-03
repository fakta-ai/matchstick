package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type pool struct {
	name     string
	last     time.Time
	draining bool
}

func (p *pool) Name() string           { return p.name }
func (p *pool) LastSuccess() time.Time { return p.last }
func (p *pool) Draining() bool         { return p.draining }
func (p *pool) SetDraining(d bool)     { p.draining = d }

func TestHealth(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clock := now
	p := &pool{name: "build"}
	h := Metrics(prometheus.NewRegistry(), []Pool{p}, 90*time.Second, func() time.Time { return clock })
	status := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}
	if status() != http.StatusOK {
		t.Error("unhealthy right after start")
	}
	clock = now.Add(2 * time.Minute)
	if status() != http.StatusServiceUnavailable {
		t.Error("healthy without any round")
	}
	p.last = clock.Add(-time.Minute)
	if status() != http.StatusOK {
		t.Error("unhealthy after a recent round")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Error("metrics")
	}
}

func TestDrain(t *testing.T) {
	p := &pool{name: "build"}
	h := Admin([]Pool{p})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pools/build/drain", strings.NewReader(`{"draining":true}`)))
	var got PoolState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.Draining || !p.draining {
		t.Fatalf("drain: %d %s", rec.Code, rec.Body)
	}
	for path, want := range map[string]int{"/pools/nope/drain": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != want {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pools/build/drain", strings.NewReader(`nope`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body: %d", rec.Code)
	}
}
