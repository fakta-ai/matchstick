// Package metrics holds matchstick's Prometheus collectors.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the collectors every pool reports to, each labelled by pool.
type Metrics struct {
	Demand           *prometheus.GaugeVec
	Machines         *prometheus.GaugeVec
	Runners          *prometheus.GaugeVec
	Creations        *prometheus.CounterVec
	CreationFailures *prometheus.CounterVec
	Deletions        *prometheus.CounterVec
	Removals         *prometheus.CounterVec
	Rounds           *prometheus.CounterVec
	RoundDuration    *prometheus.HistogramVec
	LastSuccess      *prometheus.GaugeVec
	CreationPaused   *prometheus.GaugeVec
	Draining         *prometheus.GaugeVec
}

// New registers the collectors with reg.
func New(reg prometheus.Registerer) *Metrics {
	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "matchstick", Name: name, Help: help},
			append([]string{"pool"}, labels...))
		reg.MustRegister(g)
		return g
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "matchstick", Name: name, Help: help},
			append([]string{"pool"}, labels...))
		reg.MustRegister(c)
		return c
	}
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "matchstick",
		Name: "round_duration_seconds", Help: "Time one round took: gather, plan and act.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10)}, []string{"pool"})
	reg.MustRegister(duration)
	return &Metrics{
		Demand:   gauge("demand", "Jobs assigned to the pool, waiting or running, as GitHub last reported."),
		Machines: gauge("machines", "The pool's machines by state.", "state"),
		Runners:  gauge("runners", "The pool's runner registrations by status (offline, idle, busy).", "status"),
		Creations: counter("creations_total",
			"Machines whose creation the cloud accepted."),
		CreationFailures: counter("creation_failures_total",
			"Machine creations that failed, by kind (capacity, registration, cloud).", "kind"),
		Deletions:     counter("deletions_total", "Machines deleted, by reason.", "reason"),
		Removals:      counter("registration_removals_total", "Runner registrations removed, by reason.", "reason"),
		Rounds:        counter("rounds_total", "Rounds by result (success, failure).", "result"),
		RoundDuration: duration,
		LastSuccess: gauge("last_success_timestamp_seconds",
			"When the pool's last round that could see its machines ended, in seconds since the Unix epoch."),
		CreationPaused: gauge("creation_paused", "1 while the pool backs off creating machines."),
		Draining:       gauge("draining", "1 while the pool drains."),
	}
}
