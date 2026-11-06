package azure

import (
	"bytes"
	"context"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/smallstep/pkcs7"

	"github.com/fakta-ai/matchstick/internal/provider"
)

// rootsPEM holds the root certificate authorities Microsoft lists for Azure; see the file's header.
//
//go:embed roots.pem
var rootsPEM []byte

const (
	// signerDomain is the DNS name, or the parent of one, that the certificate signing an attested document carries
	// in Azure's public cloud.
	signerDomain = "metadata.azure.com"
	// clockSkew is how far matchstick's clock may disagree with the document's times.
	clockSkew = 5 * time.Minute
	// documentTime is the layout of the document's timeStamp fields, such as "11/30/20 21:19:24 -0000".
	documentTime = "01/02/06 15:04:05 -0700"
	// chainSteps bounds the walk from the signer to a root; issuerDownloads bounds the downloads one check makes.
	chainSteps      = 8
	issuerDownloads = 4
	// issuerLimit and issuerTimeout bound one download; issuersKept bounds the intermediates kept in memory.
	issuerLimit   = 64 << 10
	issuerTimeout = 10 * time.Second
	issuersKept   = 32
)

// issuerHosts are the hosts Microsoft lists as serving the certificates of Azure's certificate authorities (Authority
// Information Access). Intermediates are downloaded from nowhere else.
var issuerHosts = map[string]bool{
	"cacerts.digicert.com":    true,
	"cacerts.digicert.cn":     true,
	"cacerts.geotrust.com":    true,
	"caissuers.microsoft.com": true,
	"www.microsoft.com":       true,
}

// attestedProof is the attested document as IMDS returns it.
type attestedProof struct {
	Encoding  string `json:"encoding"`
	Signature string `json:"signature"`
}

// attestedDocument is the signed content of an attested document: the fields matchstick checks.
type attestedDocument struct {
	VMID           string `json:"vmId"`
	SubscriptionID string `json:"subscriptionId"`
	TimeStamp      struct {
		CreatedOn string `json:"createdOn"`
		ExpiresOn string `json:"expiresOn"`
	} `json:"timeStamp"`
}

// attestation checks attested documents against a set of roots, downloading missing intermediates.
type attestation struct {
	roots    []*x509.Certificate
	pool     *x509.CertPool
	download func(ctx context.Context, url string) ([]byte, error)

	mu      sync.Mutex
	issuers map[string]*x509.Certificate // downloaded intermediates, by URL
}

func newAttestation(roots []*x509.Certificate, download func(ctx context.Context, url string) ([]byte, error)) *attestation {
	pool := x509.NewCertPool()
	for _, r := range roots {
		pool.AddCert(r)
	}
	return &attestation{roots: roots, pool: pool, download: download, issuers: map[string]*x509.Certificate{}}
}

// azureRoots returns the embedded roots.
func azureRoots() ([]*x509.Certificate, error) {
	var roots []*x509.Certificate
	rest := rootsPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("azure: embedded root certificate: %w", err)
		}
		roots = append(roots, c)
	}
	if len(roots) == 0 {
		return nil, errors.New("azure: no embedded root certificates")
	}
	return roots, nil
}

func notProven(format string, args ...any) error {
	return fmt.Errorf("%w: %s", provider.ErrNotProven, fmt.Sprintf(format, args...))
}

// check verifies an attested document and returns its content. It fails with ErrNotProven unless the document is
// signed by a certificate for metadata.azure.com chaining to a root, is current at now, and names subscription; with
// another error if an intermediate could not be downloaded.
func (a *attestation) check(ctx context.Context, proof json.RawMessage, subscription string,
	now time.Time) (attestedDocument, error) {
	var p attestedProof
	if err := json.Unmarshal(proof, &p); err != nil {
		return attestedDocument{}, notProven("not an attested document: %v", err)
	}
	if p.Encoding != "pkcs7" {
		return attestedDocument{}, notProven("encoding %q, want pkcs7", p.Encoding)
	}
	der, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return attestedDocument{}, notProven("signature: %v", err)
	}
	p7, err := pkcs7.Parse(der)
	if err != nil {
		return attestedDocument{}, notProven("signature: %v", err)
	}
	signer := p7.GetOnlySigner()
	if signer == nil || len(p7.Signers) != 1 {
		return attestedDocument{}, notProven("want exactly one signer")
	}
	if err := p7.Verify(); err != nil {
		return attestedDocument{}, notProven("signature: %v", err)
	}
	if !slices.ContainsFunc(signer.DNSNames, func(n string) bool {
		n = strings.ToLower(n)
		return n == signerDomain || strings.HasSuffix(n, "."+signerDomain)
	}) {
		return attestedDocument{}, notProven("signed by %q, not a certificate for %s", signer.Subject, signerDomain)
	}

	var doc attestedDocument
	if err := json.Unmarshal(p7.Content, &doc); err != nil {
		return attestedDocument{}, notProven("content: %v", err)
	}
	created, err := time.Parse(documentTime, doc.TimeStamp.CreatedOn)
	if err != nil {
		return attestedDocument{}, notProven("createdOn: %v", err)
	}
	expires, err := time.Parse(documentTime, doc.TimeStamp.ExpiresOn)
	if err != nil {
		return attestedDocument{}, notProven("expiresOn: %v", err)
	}
	switch {
	case expires.Before(created):
		return attestedDocument{}, notProven("expires before it was created")
	case now.After(expires.Add(clockSkew)):
		return attestedDocument{}, notProven("expired at %s", expires.UTC().Format(time.RFC3339))
	case created.After(now.Add(clockSkew)):
		return attestedDocument{}, notProven("created in the future, at %s", created.UTC().Format(time.RFC3339))
	case doc.VMID == "":
		return attestedDocument{}, notProven("no vmId")
	case !strings.EqualFold(doc.SubscriptionID, subscription):
		return attestedDocument{}, notProven("subscription %q is not the configured one", doc.SubscriptionID)
	}

	var carried []*x509.Certificate
	for _, c := range p7.Certificates {
		if c != signer {
			carried = append(carried, c)
		}
	}
	if err := a.verifyChain(ctx, signer, carried, now); err != nil {
		return attestedDocument{}, err
	}
	return doc, nil
}

// verifyChain verifies that leaf chains to a root at now. Intermediates come from carried, from earlier downloads, or
// from the Authority Information Access URLs of the certificates on the way up.
func (a *attestation) verifyChain(ctx context.Context, leaf *x509.Certificate, carried []*x509.Certificate,
	now time.Time) error {
	known := slices.Clone(carried)
	a.mu.Lock()
	for _, c := range a.issuers {
		known = append(known, c)
	}
	a.mu.Unlock()

	downloads := 0
	cur := leaf
	for range chainSteps {
		if slices.ContainsFunc(a.roots, func(r *x509.Certificate) bool { return issuedBy(cur, r) }) {
			break
		}
		if i := slices.IndexFunc(known, func(c *x509.Certificate) bool { return issuedBy(cur, c) }); i >= 0 {
			cur = known[i]
			continue
		}
		if downloads == issuerDownloads {
			break
		}
		downloads++
		parent, err := a.issuerOf(ctx, cur)
		if err != nil {
			return err
		}
		known = append(known, parent)
		cur = parent
	}

	intermediates := x509.NewCertPool()
	for _, c := range known {
		intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: a.pool, Intermediates: intermediates, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return notProven("certificate chain: %v", err)
	}
	return nil
}

// issuedBy reports whether parent signed child.
func issuedBy(child, parent *x509.Certificate) bool {
	return bytes.Equal(child.RawIssuer, parent.RawSubject) && child.CheckSignatureFrom(parent) == nil
}

// issuerOf downloads the certificate that signed c from c's Authority Information Access URLs on the allowed hosts,
// and keeps it for later checks. A download that fails is an error other than ErrNotProven, so the claim can be tried
// again.
func (a *attestation) issuerOf(ctx context.Context, c *x509.Certificate) (*x509.Certificate, error) {
	var failures []error
	for _, u := range c.IssuingCertificateURL {
		if !allowedIssuerURL(u) {
			continue
		}
		b, err := a.download(ctx, u)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		parent, err := parseCertificate(b)
		if err != nil || !issuedBy(c, parent) {
			continue
		}
		a.mu.Lock()
		if len(a.issuers) < issuersKept {
			a.issuers[u] = parent
		}
		a.mu.Unlock()
		return parent, nil
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("azure: downloading the issuer of %q: %w", c.Subject, errors.Join(failures...))
	}
	return nil, notProven("no issuer of %q found at its allowed issuer URLs", c.Subject)
}

func allowedIssuerURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Port() == "" && u.User == nil &&
		issuerHosts[strings.ToLower(u.Hostname())]
}

// parseCertificate reads one certificate, DER or PEM.
func parseCertificate(b []byte) (*x509.Certificate, error) {
	if block, _ := pem.Decode(b); block != nil {
		b = block.Bytes
	}
	return x509.ParseCertificate(b)
}

// httpDownload fetches intermediates over HTTP, from the allowed hosts only, redirects included.
func httpDownload() func(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{
		Timeout: issuerTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !allowedIssuerURL(req.URL.String()) {
				return fmt.Errorf("redirect to %s refused", req.URL.Redacted())
			}
			return nil
		},
	}
	return func(ctx context.Context, u string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", u, resp.Status)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, issuerLimit+1))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
		if len(b) > issuerLimit {
			return nil, fmt.Errorf("%s: larger than %d bytes", u, issuerLimit)
		}
		return b, nil
	}
}
