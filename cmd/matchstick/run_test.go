package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func TestRunServesHealthMetricsDrainAndClaims(t *testing.T) {
	metricsAddr, adminAddr, claimAddr := freeAddr(t), freeAddr(t), freeAddr(t)
	example, err := os.ReadFile("../../examples/fake.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(example), "instance: demo\n",
		"instance: demo\nlisten: {metrics: \""+metricsAddr+"\", admin: \""+adminAddr+"\", claim: \""+claimAddr+"\"}\n"+
			"claimURL: https://"+claimAddr+"/claim\n", 1)
	path := filepath.Join(t.TempDir(), "matchstick.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- run(ctx, path, secretSource{}, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	get := func(url string) (int, string) {
		for range 100 {
			resp, err := http.Get(url)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				return resp.StatusCode, string(b)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s never answered", url)
		return 0, ""
	}
	if code, _ := get("http://" + metricsAddr + "/healthz"); code != http.StatusOK {
		t.Errorf("healthz %d", code)
	}
	if _, body := get("http://" + metricsAddr + "/metrics"); !strings.Contains(body, `matchstick_rounds_total{pool="build",result="success"}`) {
		t.Errorf("metrics lack rounds:\n%s", body)
	}
	// The claim endpoint speaks TLS on its own address; this machine name is no pool's.
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := insecure.Post("https://"+claimAddr+"/claim", "application/json",
		strings.NewReader(`{"machine": "demo-build-0000009zzzz", "proof": {"id": "vm-1"}}`))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("claim: %s", resp.Status)
	}
	code, stdout, stderr := call(t, "drain", "--config", path, "--pool", "build", "--json")
	var out DrainOutput
	if code != 0 || json.Unmarshal([]byte(stdout), &out) != nil || !out.Draining {
		t.Errorf("drain: %d %s %s", code, stdout, stderr)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if code, _, _ := call(t, "drain", "--config", path, "--pool", "build", "--json"); code != 1 {
		t.Errorf("drain without a running process: exit %d, want 1", code)
	}
}
