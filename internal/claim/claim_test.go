package claim_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fakta-ai/matchstick/internal/claim"
	githubfake "github.com/fakta-ai/matchstick/internal/github/fake"
	"github.com/fakta-ai/matchstick/internal/metrics"
	"github.com/fakta-ai/matchstick/internal/plan"
	"github.com/fakta-ai/matchstick/internal/pool"
	providerfake "github.com/fakta-ai/matchstick/internal/provider/fake"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// endpoint serves the claim endpoint for one pool with one machine, on a local address, and returns the URL, the
// machine's bootstrap and the fake cloud.
func endpoint(t *testing.T) (pool.Bootstrap, *providerfake.Provider) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now }
	e, err := claim.NewEndpoint("https://"+l.Addr().String()+"/claim", now)
	if err != nil {
		t.Fatal(err)
	}
	machines := providerfake.New(clock)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := pool.New(pool.Settings{Instance: "ci", Name: "build", Limits: plan.Limits{Maximum: 1,
		RegistrationTimeout: plan.Duration(10 * time.Minute)}, MaxLifetime: time.Hour, Interval: time.Minute,
		Claim: e.Settings()}, machines, githubfake.New(clock), metrics.New(prometheus.NewRegistry()), log, clock)
	p.SetDemand(1)
	if _, err := p.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
	ms, err := machines.List(context.Background())
	if err != nil || len(ms) != 1 {
		t.Fatalf("machines %v, %v", ms, err)
	}
	var b pool.Bootstrap
	if err := json.Unmarshal(machines.Bootstrap(ms[0].Name), &b); err != nil {
		t.Fatal(err)
	}
	s := e.Server([]*pool.Pool{p}, log)
	go func() { _ = s.Serve(tls.NewListener(l, s.TLSConfig)) }()
	t.Cleanup(func() { _ = s.Close() })
	return b, machines
}

// client trusts only the bootstrap's certificate, as a machine does.
func client(t *testing.T, b pool.Bootstrap) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(b.ClaimCertificate)) {
		t.Fatal("the bootstrap's certificate is not PEM")
	}
	return &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}

func post(t *testing.T, c *http.Client, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func request(machine string, proof json.RawMessage) string {
	b, _ := json.Marshal(claim.Request{Machine: machine, Proof: proof})
	return string(b)
}

func code(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func TestClaimOverTLS(t *testing.T) {
	b, machines := endpoint(t)
	c := client(t, b)
	status, out := post(t, c, b.ClaimURL, request(b.Machine, machines.Proof(b.Machine)))
	if status != http.StatusOK || out["jitConfig"] != "jit:"+b.Machine {
		t.Fatalf("claim: %d %v", status, out)
	}
	if status, out := post(t, c, b.ClaimURL, request(b.Machine, machines.Proof(b.Machine))); status != http.StatusForbidden ||
		code(out) != claim.CodeRefused {
		t.Errorf("second claim: %d %v", status, out)
	}
}

func TestClaimAnswers(t *testing.T) {
	b, machines := endpoint(t)
	c := client(t, b)
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"not JSON", "{", http.StatusBadRequest, claim.CodeInvalidInput},
		{"no proof", `{"machine": "` + b.Machine + `"}`, http.StatusBadRequest, claim.CodeInvalidInput},
		{"unknown field", `{"machine": "x", "proof": {}, "jit": 1}`, http.StatusBadRequest, claim.CodeInvalidInput},
		{"too large", request(b.Machine, json.RawMessage(`"`+strings.Repeat("a", claim.MaxRequest)+`"`)),
			http.StatusBadRequest, claim.CodeInvalidInput},
		{"another pool's machine", request("ci-other-0000000aaaa", machines.Proof(b.Machine)), http.StatusForbidden,
			claim.CodeRefused},
		{"another machine's proof", request(b.Machine, machines.Proof("ci-build-0000000zzzz")), http.StatusForbidden,
			claim.CodeRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, out := post(t, c, b.ClaimURL, tc.body); status != tc.status || code(out) != tc.code {
				t.Errorf("%d %v, want %d %s", status, out, tc.status, tc.code)
			}
		})
	}
	machines.Fail("identify", errors.New("the cloud is unavailable"))
	if status, out := post(t, c, b.ClaimURL, request(b.Machine, machines.Proof(b.Machine))); status != http.StatusServiceUnavailable ||
		code(out) != claim.CodeUnavailable {
		t.Errorf("claim while the cloud is unavailable: %d %v", status, out)
	}
	if status, out := post(t, c, b.ClaimURL, request(b.Machine, machines.Proof(b.Machine))); status != http.StatusOK {
		t.Errorf("claim after a retryable failure: %d %v", status, out)
	}
}

func TestMachineTrustsOnlyItsBootstrapsCertificate(t *testing.T) {
	b, machines := endpoint(t)
	// Another endpoint's certificate, as after a restart or from a host posing as matchstick.
	other, err := claim.NewEndpoint(b.ClaimURL, now)
	if err != nil {
		t.Fatal(err)
	}
	stranger := b
	stranger.ClaimCertificate = other.Settings().Certificate
	_, err = client(t, stranger).Post(b.ClaimURL, "application/json",
		bytes.NewReader([]byte(request(b.Machine, machines.Proof(b.Machine)))))
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("post with another certificate: %v, want an unknown authority", err)
	}
	// Nothing was released: the machine still gets its configuration from the right endpoint.
	if status, out := post(t, client(t, b), b.ClaimURL, request(b.Machine, machines.Proof(b.Machine))); status != http.StatusOK {
		t.Errorf("claim: %d %v", status, out)
	}
}

func TestEndpointCertificateNamesTheHost(t *testing.T) {
	for _, host := range []string{"matchstick.runners.example", "10.1.2.3"} {
		e, err := claim.NewEndpoint("https://"+host+":8443/claim", now)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode([]byte(e.Settings().Certificate))
		if block == nil {
			t.Fatalf("%s: certificate is not PEM", host)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: roots,
			CurrentTime: now.Add(10 * 365 * 24 * time.Hour)}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	if _, err := claim.NewEndpoint("http://matchstick.example/claim", now); err == nil {
		t.Error("an http URL was accepted")
	}
}
