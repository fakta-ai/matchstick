package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/fakta-ai/matchstick/internal/cli"
	"github.com/fakta-ai/matchstick/internal/metrics"
	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/pool"
	"github.com/fakta-ai/matchstick/internal/server"
)

func runCommand(stderr io.Writer) cli.Command {
	return cli.Command{
		Name:    "run",
		Summary: "Run the pools until stopped: create a machine per job, delete it afterwards.",
		Run: func(ctx context.Context, args []string) int {
			flags := flag.NewFlagSet("run", flag.ContinueOnError)
			flags.SetOutput(stderr)
			configPath := flags.String("config", defaultConfig, "the configuration file")
			secrets := flags.String("secrets", "",
				"'-' to read the GitHub credential from standard input as JSON, instead of the secret mount")
			secretsDir := flags.String("secrets-dir", "", "the secret mount (default /var/run/secrets/matchstick)")
			flags.Usage = func() {
				fmt.Fprintf(stderr, "Run the pools until stopped (SIGINT or SIGTERM).\n\nUsage:\n  matchstick run [flags]\n\n"+
					"Flags:\n")
				flags.PrintDefaults()
				fmt.Fprintf(stderr, "\nExample:\n  matchstick run --config /etc/matchstick/matchstick.yaml\n")
			}
			if err := flags.Parse(args); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return cli.ExitOK
				}
				return cli.ExitInvalid
			}
			if *secrets != "" && *secrets != "-" {
				fmt.Fprintln(stderr, "matchstick: --secrets takes only '-'")
				return cli.ExitInvalid
			}
			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			if err := run(ctx, *configPath, secretSource{stdin: *secrets == "-", dir: *secretsDir}, log); err != nil {
				log.Error("stopped", "error", err)
				if e := op.AsError(err); e.Code == op.CodeInvalidConfig || e.Code == op.CodeInvalidInput {
					return cli.ExitInvalid
				}
				return cli.ExitFailure
			}
			return cli.ExitOK
		},
	}
}

// run serves every pool until ctx ends.
func run(ctx context.Context, configPath string, secrets secretSource, log *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	backends, err := newBackends(ctx, cfg, cfg.Pools, secrets, true, log)
	if err != nil {
		return err
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)
	var pools []*pool.Pool
	var served []server.Pool
	for _, pc := range cfg.Pools {
		p := pool.New(poolSettings(cfg, pc), backends[pc.Name].machines, backends[pc.Name].runners, m, log, now)
		pools = append(pools, p)
		served = append(served, p)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	interval := time.Duration(cfg.Interval)
	servers := []*http.Server{
		{Addr: cfg.Listen.Metrics, Handler: server.Metrics(reg, served, 3*interval, now),
			ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.Listen.Admin, Handler: server.Admin(served), ReadHeaderTimeout: 10 * time.Second},
	}
	var listeners []net.Listener
	for _, s := range servers {
		l, err := net.Listen("tcp", s.Addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return op.Errorf(op.CodeInvalidConfig, "listen on %s: %v", s.Addr, err)
		}
		listeners = append(listeners, l)
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(servers))
	for i, s := range servers {
		wg.Go(func() {
			if err := s.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				cancel()
			}
		})
	}
	for i, pc := range cfg.Pools {
		p, be := pools[i], backends[pc.Name]
		wg.Go(func() { p.Run(ctx) })
		if be.listen != nil {
			wg.Go(func() { be.listen(ctx, p) })
		} else {
			demand, err := be.demand(ctx)
			if err == nil && demand != nil {
				p.SetDemand(*demand)
			}
		}
	}
	log.Info("running", "pools", len(pools), "metrics", cfg.Listen.Metrics, "admin", cfg.Listen.Admin,
		"version", version)

	<-ctx.Done()
	shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}
