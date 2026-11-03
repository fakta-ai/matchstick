package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v5"

	"github.com/fakta-ai/matchstick/internal/naming"
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
	ours := naming.New("ci", "build", time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	var entries []restRunner
	for i := range 150 {
		entries = append(entries, restRunner{ID: int64(i + 1), Name: fmt.Sprintf("someone-else-%d", i), Status: "online"})
	}
	entries[120] = restRunner{ID: 121, Name: ours, Status: "online", Busy: true}
	entries[3] = restRunner{ID: 4, Name: naming.New("ci", "test", time.Now()), Status: "online"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/actions/runners" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((page-1)*100, len(entries))
		end := min(start+100, len(entries))
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(entries), "runners": entries[start:end]})
	}))
	defer srv.Close()
	s := &ScaleSet{instance: "ci", pool: "build", client: &Client{rest: &restClient{api: srv.URL,
		runnersPath: "/orgs/acme/actions/runners", creds: Credentials{Token: "tok"}, http: srv.Client()}}}
	got, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 121 || !got[0].Online || !got[0].Busy ||
		!got[0].CreatedAt.Equal(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("got %+v", got)
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
