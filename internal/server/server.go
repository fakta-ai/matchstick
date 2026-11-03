// Package server serves matchstick's HTTP endpoints: metrics and health on one address, the admin endpoint (drain)
// on another, meant to stay on a loopback address.
package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Pool is what the endpoints need from a pool.
type Pool interface {
	Name() string
	LastSuccess() time.Time
	Draining() bool
	SetDraining(bool)
}

// PoolState is one pool as the admin endpoint reports it.
type PoolState struct {
	Pool        string    `json:"pool"`
	Draining    bool      `json:"draining"`
	LastSuccess time.Time `json:"lastSuccess"`
}

// DrainRequest asks to start or stop draining a pool.
type DrainRequest struct {
	Draining bool `json:"draining"`
}

// Metrics returns the handler for /metrics and /healthz. A pool is healthy while its last successful round ended
// within stale; before its first, while the process is younger than stale.
func Metrics(gatherer prometheus.Gatherer, pools []Pool, stale time.Duration, now func() time.Time) http.Handler {
	started := now()
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		t := now()
		for _, p := range pools {
			last := p.LastSuccess()
			if last.IsZero() {
				last = started
			}
			if t.Sub(last) > stale {
				http.Error(w, "pool "+p.Name()+": no successful round since "+last.Format(time.RFC3339),
					http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Admin returns the handler for the admin endpoint: GET /pools, and POST /pools/{pool}/drain with a DrainRequest.
func Admin(pools []Pool) http.Handler {
	byName := map[string]Pool{}
	for _, p := range pools {
		byName[p.Name()] = p
	}
	state := func(p Pool) PoolState {
		return PoolState{Pool: p.Name(), Draining: p.Draining(), LastSuccess: p.LastSuccess()}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pools", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]PoolState, 0, len(pools))
		for _, p := range pools {
			out = append(out, state(p))
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /pools/{pool}/drain", func(w http.ResponseWriter, r *http.Request) {
		p, ok := byName[r.PathValue("pool")]
		if !ok {
			http.Error(w, "no such pool", http.StatusNotFound)
			return
		}
		var req DrainRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
			http.Error(w, "body must be {\"draining\": true|false}", http.StatusBadRequest)
			return
		}
		p.SetDraining(req.Draining)
		writeJSON(w, state(p))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
