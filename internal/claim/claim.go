// Package claim serves the claim endpoint: a machine posts proof of which machine it is, and gets its runner's JIT
// configuration, once. The endpoint speaks TLS with a key made at start and held only in memory; machines trust only
// its certificate, which they get in their bootstrap.
package claim

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/fakta-ai/matchstick/internal/pool"
)

const (
	// MaxRequest bounds a claim's body.
	MaxRequest = 64 << 10
	// Timeout bounds the handling of one claim, the proof's check included.
	Timeout = 30 * time.Second
)

// Request is a claim: the machine's name and its cloud's proof that the sender is that machine.
type Request struct {
	Machine string          `json:"machine"`
	Proof   json.RawMessage `json:"proof"`
}

// Response carries the runner's JIT configuration.
type Response struct {
	JITConfig string `json:"jitConfig"`
}

// Error codes in the error object of a failed claim.
const (
	// CodeInvalidInput: the request is not a claim (400).
	CodeInvalidInput = "invalid_input"
	// CodeRefused: nothing will be released to this machine (403); final.
	CodeRefused = "refused"
	// CodeUnavailable: the proof could not be checked now (503); try again.
	CodeUnavailable = "unavailable"
)

// Handler serves claims posted to path, for the given pools.
func Handler(path string, pools []*pool.Pool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var req Request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequest))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil || req.Machine == "" || len(req.Proof) == 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidInput,
				`the body must be {"machine": "<name>", "proof": <the cloud's document>}`)
			return
		}
		var owner *pool.Pool
		for _, p := range pools {
			if p.Owns(req.Machine) {
				owner = p
				break
			}
		}
		if owner == nil {
			log.Warn("claim refused", "machine", req.Machine, "why", "no pool owns this name")
			writeError(w, http.StatusForbidden, CodeRefused, "refused")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), Timeout)
		defer cancel()
		config, err := owner.Claim(ctx, req.Machine, req.Proof)
		switch {
		case errors.Is(err, pool.ErrRefused):
			writeError(w, http.StatusForbidden, CodeRefused, "refused")
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "the proof could not be checked now; try again")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(Response{JITConfig: config})
		}
	})
	return mux
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Error.Code, body.Error.Message = code, message
	_ = json.NewEncoder(w).Encode(body)
}

// Endpoint is the claim endpoint at one URL, with its key.
type Endpoint struct {
	url     string
	path    string
	cert    tls.Certificate
	certPEM string
}

// NewEndpoint makes a fresh key and a self-signed certificate naming rawURL's host (a DNS name or an IP address), for
// the endpoint at rawURL. The key is never written anywhere.
func NewEndpoint(rawURL string, now time.Time) (*Endpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, fmt.Errorf("claimURL %q: want https://<host>[:<port>][/<path>]", rawURL)
	}
	host := u.Hostname()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("claim: key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("claim: serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		// No expiry (RFC 5280, 4.1.2.5): the certificate lives as long as the process that holds its key.
		NotAfter:              time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("claim: certificate: %w", err)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return &Endpoint{url: rawURL, path: path, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, nil
}

// Settings returns the endpoint as machines see it: its URL and certificate, for their bootstrap.
func (e *Endpoint) Settings() pool.Claim {
	return pool.Claim{URL: e.url, Certificate: e.certPEM}
}

// Server is the endpoint's HTTP server, with the listener to serve it on.
type Server struct {
	*http.Server
	handshakes *handshakes
}

// Server returns the endpoint's server, for the given pools. Its error log goes to log, except a TLS handshake that
// ends at once on a connection that sent no byte, as a load balancer's TCP health probe does: that calls probed
// instead. Serve it on Listener.
func (e *Endpoint) Server(pools []*pool.Pool, log *slog.Logger, probed func()) *Server {
	h := &handshakes{log: log, probed: probed, open: map[string]*countingConn{}}
	return &Server{handshakes: h, Server: &http.Server{
		Handler:           Handler(e.path, pools, log),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{e.cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      Timeout + 30*time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          stdlog.New(h, "", 0),
	}}
}

// Listener wraps l, a TCP listener, into the one to serve on: TLS with the endpoint's key, over connections that count
// what they read.
func (s *Server) Listener(l net.Listener) net.Listener {
	return tls.NewListener(&countingListener{Listener: l, h: s.handshakes}, s.TLSConfig)
}
