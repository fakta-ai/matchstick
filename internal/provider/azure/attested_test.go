package azure

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fakta-ai/matchstick/internal/provider"
)

// testPKI is a throwaway certificate hierarchy shaped like Azure's: a root, an intermediate the documents do not
// carry (served at the leaf's issuer URL), and a leaf for metadata.azure.com that signs documents.
type testPKI struct {
	root, intermediate, leaf *x509.Certificate
	leafKey                  *rsa.PrivateKey
	intermediateKey          *rsa.PrivateKey
	intermediateURL          string
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, template, parent *x509.Certificate, pub *rsa.PublicKey, priv *rsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func caTemplate(name string, serial int64) *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
}

// newPKI makes a hierarchy whose leaf carries the given DNS names.
func newPKI(t *testing.T, dnsNames ...string) *testPKI {
	t.Helper()
	rootKey, interKey, leafKey := newKey(t), newKey(t), newKey(t)
	root := sign(t, caTemplate("Test Root", 1), caTemplate("Test Root", 1), &rootKey.PublicKey, rootKey)
	inter := sign(t, caTemplate("Test Issuing CA", 2), root, &interKey.PublicKey, rootKey)
	url := "http://caissuers.microsoft.com/test/Test%20Issuing%20CA.crt"
	leaf := sign(t, &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: dnsNames[0]},
		DNSNames: dnsNames, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IssuingCertificateURL: []string{"http://intranet.example/x.crt", url}}, inter, &leafKey.PublicKey, interKey)
	return &testPKI{root: root, intermediate: inter, leaf: leaf, leafKey: leafKey, intermediateKey: interKey,
		intermediateURL: url}
}

// downloads serves the PKI's intermediate at its URL, counting calls; fail makes the next calls fail.
type downloads struct {
	mu    sync.Mutex
	pki   *testPKI
	calls []string
	fail  int
}

func (d *downloads) get(_ context.Context, url string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, url)
	if d.fail > 0 {
		d.fail--
		return nil, errors.New("connection refused")
	}
	if url == d.pki.intermediateURL {
		return d.pki.intermediate.Raw, nil
	}
	return nil, errors.New("404 Not Found")
}

// Object identifiers of the PKCS#7 structure Azure's documents use.
var (
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidData          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSHA256WithRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidRSA           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
)

type algorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue
}

var null = asn1.RawValue{Tag: asn1.TagNull}

type issuerAndSerial struct {
	Issuer asn1.RawValue
	Serial *big.Int
}

type signerInfo struct {
	Version            int
	IssuerAndSerial    issuerAndSerial
	DigestAlgorithm    algorithm
	EncryptionAlgoritm algorithm
	EncryptedDigest    []byte
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"optional"`
}

type signedData struct {
	Version          int
	DigestAlgorithms []algorithm `asn1:"set"`
	ContentInfo      contentInfo
	Certificates     asn1.RawValue `asn1:"optional,tag:0"`
	SignerInfos      []signerInfo  `asn1:"set"`
}

// document builds an attested document as IMDS returns it: content signed by the leaf, the leaf (and any extra
// certificates) attached, and the digest algorithm given as sha256WithRSAEncryption, as Azure does.
func (p *testPKI) document(t *testing.T, content string, extra ...*x509.Certificate) json.RawMessage {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.leafKey, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	octets, err := asn1.Marshal([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	certs := slices.Clone(p.leaf.Raw)
	for _, c := range extra {
		certs = append(certs, c.Raw...)
	}
	sd := signedData{
		Version:          1,
		DigestAlgorithms: []algorithm{{Algorithm: oidSHA256WithRSA, Parameters: null}},
		ContentInfo:      contentInfo{ContentType: oidData, Content: explicit0(octets)},
		Certificates:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: certs},
		SignerInfos: []signerInfo{{
			Version:            1,
			IssuerAndSerial:    issuerAndSerial{Issuer: asn1.RawValue{FullBytes: p.leaf.RawIssuer}, Serial: p.leaf.SerialNumber},
			DigestAlgorithm:    algorithm{Algorithm: oidSHA256WithRSA, Parameters: null},
			EncryptionAlgoritm: algorithm{Algorithm: oidRSA, Parameters: null},
			EncryptedDigest:    sig,
		}},
	}
	inner, err := asn1.Marshal(sd)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := asn1.Marshal(contentInfo{ContentType: oidSignedData, Content: explicit0(inner)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(attestedProof{Encoding: "pkcs7", Signature: base64.StdEncoding.EncodeToString(outer)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// explicit0 wraps DER in an explicit [0] tag.
func explicit0(der []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der}
}

const vmID = "6f1e3c51-0000-4000-8000-000000000001"

// content is a document's signed content, created at created and valid for six hours, as Azure signs them.
func content(vm, subscription string, created time.Time) string {
	layout := "01/02/06 15:04:05 -0000"
	b, _ := json.Marshal(map[string]any{
		"nonce": "20261103-120000", "plan": map[string]string{"name": "", "product": "", "publisher": ""},
		"sku": "", "subscriptionId": subscription, "vmId": vm, "licenseType": "",
		"timeStamp": map[string]string{"createdOn": created.UTC().Format(layout),
			"expiresOn": created.UTC().Add(6 * time.Hour).Format(layout)},
	})
	return string(b)
}

// attestedProvider is a provider over a fake resource group holding VM ci-build-a of this pool, with vmId vmID, and
// VM ci-other-a of another pool, checking documents against the PKI's root.
func attestedProvider(t *testing.T, pki *testPKI) (*Provider, *downloads, *azureWorld) {
	t.Helper()
	p, w := newProvider(t, shape)
	for _, name := range []string{"ci-build-a", "ci-other-a"} {
		if err := create(t, p, name); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	for name, vm := range w.vms {
		vm.Properties.VMID = new(vmID)
		if name == "ci-other-a" {
			vm.Properties.VMID = new("6f1e3c51-0000-4000-8000-000000000002")
			vm.Tags[TagPool] = new("other")
		}
		w.vms[name] = vm
	}
	w.mu.Unlock()
	d := &downloads{pki: pki}
	p.attest = newAttestation([]*x509.Certificate{pki.root}, d.get)
	return p, d, w
}

func TestIdentifyAcceptsTheVMsOwnDocument(t *testing.T) {
	pki := newPKI(t, "eastus.metadata.azure.com", "metadata.azure.com")
	p, d, _ := attestedProvider(t, pki)
	doc := pki.document(t, content(vmID, cloud.SubscriptionID, now.Add(-time.Minute)))
	if err := p.Identify(context.Background(), "ci-build-a", doc); err != nil {
		t.Fatalf("identify: %v", err)
	}
	// The intermediate was downloaded from the allowed host only, and is kept for the next check.
	if len(d.calls) != 1 || d.calls[0] != pki.intermediateURL {
		t.Errorf("downloads %v", d.calls)
	}
	if err := p.Identify(context.Background(), "ci-build-a", doc); err != nil || len(d.calls) != 1 {
		t.Errorf("second check: %v, downloads %v", err, d.calls)
	}
}

func TestIdentifyRefuses(t *testing.T) {
	pki := newPKI(t, "metadata.azure.com")
	valid := content(vmID, cloud.SubscriptionID, now.Add(-time.Minute))
	tampered := func() json.RawMessage {
		var p attestedProof
		_ = json.Unmarshal(pki.document(t, valid), &p)
		der, _ := base64.StdEncoding.DecodeString(p.Signature)
		i := strings.Index(string(der), vmID)
		der[i] ^= 1
		b, _ := json.Marshal(attestedProof{Encoding: "pkcs7", Signature: base64.StdEncoding.EncodeToString(der)})
		return b
	}
	cases := []struct {
		name    string
		machine string
		proof   func() json.RawMessage
	}{
		{"another VM's document", "ci-build-a", func() json.RawMessage {
			return pki.document(t, content("6f1e3c51-0000-4000-8000-000000000009", cloud.SubscriptionID, now))
		}},
		{"a VM of another pool", "ci-other-a", func() json.RawMessage { return pki.document(t, valid) }},
		{"a VM that does not exist", "ci-build-b", func() json.RawMessage { return pki.document(t, valid) }},
		{"another subscription", "ci-build-a", func() json.RawMessage {
			return pki.document(t, content(vmID, "11111111-1111-1111-1111-111111111111", now))
		}},
		{"an expired document", "ci-build-a", func() json.RawMessage {
			return pki.document(t, content(vmID, cloud.SubscriptionID, now.Add(-6*time.Hour-6*time.Minute)))
		}},
		{"a document from the future", "ci-build-a", func() json.RawMessage {
			return pki.document(t, content(vmID, cloud.SubscriptionID, now.Add(6*time.Minute)))
		}},
		{"an altered document", "ci-build-a", tampered},
		{"a signer not for metadata.azure.com", "ci-build-a", func() json.RawMessage {
			return pki.reissue(t, []string{"metadata.azure.com.example.net", "evilmetadata.azure.com"},
				pki.intermediateURL).document(t, valid)
		}},
		{"a chain to an unknown root", "ci-build-a", func() json.RawMessage {
			stranger := newPKI(t, "metadata.azure.com")
			return stranger.document(t, valid, stranger.intermediate, stranger.root)
		}},
		{"an issuer URL serving another CA", "ci-build-a", func() json.RawMessage {
			return newPKI(t, "metadata.azure.com").document(t, valid)
		}},
		{"another encoding", "ci-build-a", func() json.RawMessage { return json.RawMessage(`{"encoding":"jws","signature":""}`) }},
		{"not a document", "ci-build-a", func() json.RawMessage { return json.RawMessage(`"hello"`) }},
		{"not PKCS#7", "ci-build-a", func() json.RawMessage {
			return json.RawMessage(`{"encoding":"pkcs7","signature":"` + base64.StdEncoding.EncodeToString(pki.leaf.Raw) + `"}`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, _, _ := attestedProvider(t, pki)
			if err := p.Identify(context.Background(), c.machine, c.proof()); !errors.Is(err, provider.ErrNotProven) {
				t.Errorf("identify: %v, want ErrNotProven", err)
			}
		})
	}
}

func TestIdentifyDownloadsOnlyFromMicrosoftsIssuerHosts(t *testing.T) {
	pki := newPKI(t, "metadata.azure.com")
	p, d, _ := attestedProvider(t, pki)
	outside := pki.reissue(t, []string{"metadata.azure.com"}, "http://169.254.169.254/x",
		"http://caissuers.microsoft.com:8080/x", "file:///etc/passwd", "http://caissuers.microsoft.com.example.net/x",
		"http://user@www.microsoft.com/x")
	err := p.Identify(context.Background(), "ci-build-a", outside.document(t, content(vmID, cloud.SubscriptionID, now)))
	if !errors.Is(err, provider.ErrNotProven) || len(d.calls) != 0 {
		t.Errorf("identify: %v, downloads %v", err, d.calls)
	}
}

func TestIdentifyFailsRetryablyWhenTheIssuerCannotBeDownloaded(t *testing.T) {
	pki := newPKI(t, "metadata.azure.com")
	p, d, _ := attestedProvider(t, pki)
	d.fail = 1
	doc := pki.document(t, content(vmID, cloud.SubscriptionID, now))
	err := p.Identify(context.Background(), "ci-build-a", doc)
	if err == nil || errors.Is(err, provider.ErrNotProven) {
		t.Fatalf("identify with the download failing: %v, want a retryable error", err)
	}
	if err := p.Identify(context.Background(), "ci-build-a", doc); err != nil {
		t.Errorf("identify once the download works: %v", err)
	}
}

func TestIdentifyFailsRetryablyWhenAzureCannotBeAsked(t *testing.T) {
	pki := newPKI(t, "metadata.azure.com")
	p, _, w := attestedProvider(t, pki)
	w.getErr = []string{"AuthorizationFailed"}
	err := p.Identify(context.Background(), "ci-build-a", pki.document(t, content(vmID, cloud.SubscriptionID, now)))
	if err == nil || errors.Is(err, provider.ErrNotProven) {
		t.Errorf("identify: %v, want a retryable error", err)
	}
}

func TestEmbeddedRoots(t *testing.T) {
	roots, err := azureRoots()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"DigiCert Global Root CA": true, "DigiCert Global Root G2": true, "DigiCert Global Root G3": true,
		"DigiCert TLS ECC P384 Root G5": true, "DigiCert TLS RSA4096 Root G5": true,
		"Microsoft ECC Root Certificate Authority 2017": true, "Microsoft RSA Root Certificate Authority 2017": true,
	}
	for _, r := range roots {
		if !want[r.Subject.CommonName] || !r.IsCA {
			t.Errorf("unexpected root %s", r.Subject)
		}
		delete(want, r.Subject.CommonName)
	}
	if len(want) > 0 {
		t.Errorf("missing roots %v", want)
	}
}

// reissue returns the PKI with a new leaf from its intermediate, for the same key, carrying dnsNames and issuer URLs.
func (p *testPKI) reissue(t *testing.T, dnsNames []string, urls ...string) *testPKI {
	t.Helper()
	q := *p
	q.leaf = sign(t, &x509.Certificate{SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: dnsNames[0]},
		DNSNames: dnsNames, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IssuingCertificateURL: urls}, p.intermediate, &p.leafKey.PublicKey, p.intermediateKey)
	return &q
}
