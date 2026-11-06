package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v5"

	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
)

func TestRunnersEndpoint(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://github.com/acme":               {"https://api.github.com", "/orgs/acme/actions/runners"},
		"https://github.com/acme/":              {"https://api.github.com", "/orgs/acme/actions/runners"},
		"https://github.com/acme/app":           {"https://api.github.com", "/repos/acme/app/actions/runners"},
		"https://github.com/enterprises/big":    {"https://api.github.com", "/enterprises/big/actions/runners"},
		"https://git.example.com/acme":          {"https://git.example.com/api/v3", "/orgs/acme/actions/runners"},
		"https://acme.ghe.com/enterprises/acme": {"https://api.acme.ghe.com", "/enterprises/acme/actions/runners"},
	} {
		api, path, err := runnersEndpoint(in)
		if err != nil || api != want[0] || path != want[1] {
			t.Errorf("%s: %s %s %v", in, api, path, err)
		}
	}
	for _, bad := range []string{"", "acme", "https://github.com", "https://github.com/a/b/c"} {
		if _, _, err := runnersEndpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestListKeepsOnlyThePoolsRunnersAcrossPages(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	busy := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	idle := time.Date(2026, 11, 3, 12, 30, 0, 0, time.UTC)
	var entries []restRunner
	for i := range 200 {
		entries = append(entries, restRunner{ID: int64(i + 1), Name: fmt.Sprintf("someone-else-%d", i), Status: "online"})
	}
	entries[10] = restRunner{ID: 11, Name: naming.New("ci", "build", busy), Status: "online", Busy: true}
	entries[150] = restRunner{ID: 151, Name: naming.New("ci", "build", idle), Status: "offline"}
	entries[3] = restRunner{ID: 4, Name: naming.New("ci", "test", busy), Status: "online"}          // another pool
	entries[4] = restRunner{ID: 5, Name: naming.New("other", "build", busy), Status: "online"}      // another instance
	entries[5] = restRunner{ID: 6, Name: naming.Prefix("ci", "build") + "manual", Status: "online"} // not one of ours
	f.configure(func() { f.runners = entries })
	s, err := f.client().FindScaleSet(t.Context(), "ci", "build", "linux")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []plan.Runner{
		{ID: 11, Name: entries[10].Name, CreatedAt: busy, Online: true, Busy: true},
		{ID: 151, Name: entries[150].Name, CreatedAt: idle},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if pages := f.log("list"); !slices.Equal(pages, []string{"list page=1 token=pat", "list page=2 token=pat"}) {
		t.Errorf("read %q", pages)
	}
}

func TestListFailsWhenTheRunnerListCannotBeRead(t *testing.T) {
	f := newFakeGitHub(t)
	f.addScaleSet("ci-build", nil, nil)
	f.configure(func() { f.failList = true })
	s, err := f.client().FindScaleSet(t.Context(), "ci", "build", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.List(t.Context()); err == nil {
		t.Errorf("got %+v and no error", got)
	}
}

func TestInstallationTokenIsRenewedBeforeItExpires(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeGitHub(t)
	f.configure(func() { f.appClientID, f.appKey = "Iv1.abc", key })
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	c, err := newRESTClient(f.srv.URL+"/acme",
		Credentials{AppClientID: "Iv1.abc", AppInstallationID: 42, AppPrivateKey: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	c.now = f.now
	// Each installation token lasts an hour.
	for _, step := range []struct {
		after time.Duration
		token string
	}{
		{0, "installation-1"},
		{54 * time.Minute, "installation-1"}, // six minutes left: still used
		{2 * time.Minute, "installation-2"},  // four minutes left: renewed
		{3 * time.Hour, "installation-3"},    // long expired: renewed
	} {
		f.advance(step.after)
		if _, err := c.runners(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := f.log("list"); got[len(got)-1] != "list page=1 token="+step.token {
			t.Fatalf("after %s: %q", step.after, got)
		}
	}
	if issued := f.log("installation token"); len(issued) != 3 {
		t.Errorf("issued %q", issued)
	}
}

func TestAppTokenIsSignedForTheApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, der := range map[string]func() ([]byte, string){
		"pkcs1": func() ([]byte, string) { return x509.MarshalPKCS1PrivateKey(key), "RSA PRIVATE KEY" },
		"pkcs8": func() ([]byte, string) { b, _ := x509.MarshalPKCS8PrivateKey(key); return b, "PRIVATE KEY" },
	} {
		b, typ := der()
		pemKey := string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
		signed, err := appToken(Credentials{AppClientID: "Iv1.abc", AppPrivateKey: pemKey}, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var claims jwt.RegisteredClaims
		if _, err := jwt.ParseWithClaims(signed, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithValidMethods([]string{"RS256"})); err != nil || claims.Issuer != "Iv1.abc" {
			t.Errorf("%s: %v %+v", name, err, claims)
		}
	}
	if _, err := appToken(Credentials{AppPrivateKey: "not a key"}, time.Now()); err == nil {
		t.Error("garbage key accepted")
	}
}

func TestSameLabelsIgnoresOrder(t *testing.T) {
	l := func(names ...string) (out []scaleset.Label) {
		for _, n := range names {
			out = append(out, scaleset.Label{Name: n})
		}
		return out
	}
	if !sameLabels(l("a", "b"), l("b", "a")) || sameLabels(l("a"), l("a", "b")) {
		t.Error("sameLabels")
	}
}
