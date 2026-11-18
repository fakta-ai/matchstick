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
	"sync"
	"sync/atomic"
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

// endpoint serves the claim endpoint for one pool with one machine, on a local address, and returns the machine's
// bootstrap and the fake cloud.
func endpoint(t *testing.T) (pool.Bootstrap, *providerfake.Provider) {
	t.Helper()
	s := serve(t)
	return s.bootstrap, s.machines
}

// served is a claim endpoint serving on a local address, with what it logged and how many probes it counted.
type served struct {
	bootstrap pool.Bootstrap
	machines  *providerfake.Provider
	addr      string
	log       *syncBuffer
	probes    *atomic.Int64
}

func serve(t *testing.T) served {
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
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	p := pool.New(pool.Settings{Instance: "ci", Name: "build", Limits: plan.Limits{Maximum: 1,
		RegistrationTimeout: plan.Duration(10 * time.Minute)}, MaxLifetime: time.Hour, Interval: time.Minute,
		Claim: e.Settings()}, machines, githubfake.New(clock), metrics.New(prometheus.NewRegistry()),
		slog.New(slog.DiscardHandler), clock)
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
	probes := &atomic.Int64{}
	s := e.Server([]*pool.Pool{p}, log, func() { probes.Add(1) })
	go func() { _ = s.Serve(s.Listener(l)) }()
	t.Cleanup(func() { _ = s.Close() })
	return served{bootstrap: b, machines: machines, addr: l.Addr().String(), log: logs, probes: probes}
}

// syncBuffer collects log lines written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
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

// waitUntil waits up to ten seconds for done.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("waiting for %s", what)
}

// helloOnly is a connection that keeps what a TLS client writes and reads nothing, to capture a ClientHello.
type helloOnly struct {
	net.Conn
	written bytes.Buffer
}

func (c *helloOnly) Write(p []byte) (int, error) { return c.written.Write(p) }
func (c *helloOnly) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *helloOnly) Close() error                { return nil }

func TestConnectionsClosedBeforeTheirFirstByteAreCountedAndEveryOtherHandshakeErrorIsLogged(t *testing.T) {
	s := serve(t)
	dial := func() *net.TCPConn {
		c, err := net.Dial("tcp", s.addr)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.TCPConn)
	}
	// A load balancer's TCP health probe: connect, then close.
	for range 3 {
		_ = dial().Close()
	}
	waitUntil(t, "3 probes", func() bool { return s.probes.Load() == 3 })
	if got := s.log.String(); got != "" {
		t.Errorf("probes were logged:\n%s", got)
	}

	// Real clients whose handshake fails are logged, with why, and not counted.
	// One byte, then a close: the connection ends within a TLS record.
	c := dial()
	if _, err := c.Write([]byte{0x16}); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	// A client that does not trust the endpoint's certificate.
	if conn, err := tls.Dial("tcp", s.addr, &tls.Config{MinVersion: tls.VersionTLS12}); err == nil {
		_ = conn.Close()
		t.Error("a client without the bootstrap's certificate completed the handshake")
	}
	// A whole ClientHello, then a close without an alert: an EOF at a record boundary on a connection that read
	// something.
	hello := &helloOnly{}
	_ = tls.Client(hello, &tls.Config{ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}).Handshake()
	c = dial()
	if _, err := c.Write(hello.written.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWrite()
	_, _ = io.Copy(io.Discard, c)
	_ = c.Close()

	logged := func() int { return strings.Count(s.log.String(), `msg="claim endpoint"`) }
	waitUntil(t, "3 handshake errors logged", func() bool { return logged() == 3 })
	log := s.log.String()
	for _, want := range []string{"unexpected EOF", "certificate", `: EOF"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if got := strings.Count(log, "http: TLS handshake error from 127.0.0.1:"); got != 3 {
		t.Errorf("%d handshake errors logged, want 3:\n%s", got, log)
	}
	if got := s.probes.Load(); got != 3 {
		t.Errorf("%d probes counted, want 3", got)
	}
	// A machine still claims.
	b := s.bootstrap
	if status, out := post(t, client(t, b), b.ClaimURL, request(b.Machine, s.machines.Proof(b.Machine))); status != http.StatusOK {
		t.Errorf("claim: %d %v", status, out)
	}
}
