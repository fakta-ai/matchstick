package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fakta-ai/matchstick/internal/op"
)

func TestHostAndTargets(t *testing.T) {
	for _, c := range []struct{ base, web, api string }{
		{"https://github.com", "https://github.com", "https://api.github.com"},
		{"https://github.com/", "https://github.com", "https://api.github.com"},
		{"https://acme.ghe.com", "https://acme.ghe.com", "https://api.acme.ghe.com"},
		{"https://github.acme.com", "https://github.acme.com", "https://github.acme.com/api/v3"},
	} {
		web, api, err := Host(c.base)
		if err != nil || web != c.web || api != c.api {
			t.Errorf("Host(%s) = %s, %s, %v", c.base, web, api, err)
		}
	}
	for _, bad := range []string{"github.com", "https://github.com/acme", "ftp://github.com", "https://github.com?x=1"} {
		if _, _, err := Host(bad); err == nil {
			t.Errorf("Host(%s) accepted", bad)
		}
	}

	if tg, err := NewTarget("https://github.com", "acme", ""); err != nil || tg.Kind != Organization ||
		tg.Name() != "acme" {
		t.Errorf("org: %+v %v", tg, err)
	}
	if tg, err := NewTarget("https://github.com", "", "acme/widgets"); err != nil || tg.Kind != Repository ||
		tg.Name() != "acme/widgets" {
		t.Errorf("repo: %+v %v", tg, err)
	}
	for _, in := range [][2]string{{"", ""}, {"acme", "acme/widgets"}, {"acme/widgets", ""}, {"", "acme"},
		{"", "acme/widgets/x"}, {"", "/widgets"}} {
		if _, err := NewTarget("https://github.com", in[0], in[1]); err == nil {
			t.Errorf("NewTarget(org %q, repo %q) accepted", in[0], in[1])
		}
	}

	for _, c := range []struct {
		url   string
		kind  Kind
		name  string
		api   string
		valid bool
	}{
		{"https://github.com/acme", Organization, "acme", "https://api.github.com", true},
		{"https://github.com/acme/widgets/", Repository, "acme/widgets", "https://api.github.com", true},
		{"https://github.com/enterprises/acme", Enterprise, "acme", "https://api.github.com", true},
		{"https://github.acme.com/platform", Organization, "platform", "https://github.acme.com/api/v3", true},
		{"https://github.com", "", "", "", false},
		{"https://github.com/a/b/c", "", "", "", false},
		{"acme", "", "", "", false},
	} {
		tg, err := ParseURL(c.url)
		if (err == nil) != c.valid || (c.valid && (tg.Kind != c.kind || tg.Name() != c.name || tg.API != c.api)) {
			t.Errorf("ParseURL(%s) = %+v, %v", c.url, tg, err)
		}
	}
}

func TestManifestAsksForExactlyWhatMatchstickNeeds(t *testing.T) {
	m, err := NewManifest(" acme-runners ", Organization, "http://127.0.0.1:4000/callback")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "acme-runners", "url": homepage, "redirect_url": "http://127.0.0.1:4000/callback", "public": false,
		"default_permissions": map[string]any{"organization_self_hosted_runners": "write", "metadata": "read"},
		"default_events":      []any{},
	}
	for k, v := range want {
		if fmt.Sprint(got[k]) != fmt.Sprint(v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["hook_attributes"]; ok {
		t.Error("the manifest has a webhook")
	}

	repo, err := NewManifest("acme-runners", Repository, "http://127.0.0.1:4000/callback")
	if err != nil || len(repo.DefaultPermissions) != 2 || repo.DefaultPermissions["administration"] != "write" ||
		repo.DefaultPermissions["metadata"] != "read" {
		t.Errorf("repository permissions %v (%v)", repo.DefaultPermissions, err)
	}
	if _, err := NewManifest("acme-runners", Enterprise, "x"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("enterprise: %v", err)
	}
	for _, bad := range []string{"", "  ", strings.Repeat("a", 35)} {
		if _, err := NewManifest(bad, Organization, "x"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

func TestRegisterURL(t *testing.T) {
	if got := RegisterURL("https://github.com", "acme", "s1"); got !=
		"https://github.com/organizations/acme/settings/apps/new?state=s1" {
		t.Errorf("organisation: %s", got)
	}
	if got := RegisterURL("https://github.acme.com", "", "s1"); got != "https://github.acme.com/settings/apps/new?state=s1" {
		t.Errorf("personal: %s", got)
	}
}

const pem = "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\n"

func TestParseConversion(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"id": 42, "slug": "acme-runners", "name": "acme-runners",
		"client_id": "Iv23liAcme", "html_url": "https://github.com/apps/acme-runners", "pem": pem,
		"client_secret": "s", "webhook_secret": nil, "owner": map[string]any{"login": "acme"}})
	c, err := ParseConversion(http.StatusCreated, body)
	if err != nil || c.ID != 42 || c.ClientID != "Iv23liAcme" || c.Owner.Login != "acme" || c.PEM != pem {
		t.Fatalf("%+v %v", c, err)
	}
	if got := InstallURL(c.HTMLURL); got != "https://github.com/apps/acme-runners/installations/new" {
		t.Errorf("install URL %s", got)
	}
	if s := fmt.Sprintf("%v %+v %#v %s", c, c, c, c); strings.Contains(s, "MIIE") {
		t.Errorf("the key printed: %s", s)
	}

	if _, err := ParseConversion(http.StatusNotFound, []byte(`{"message":"Not Found"}`)); err == nil ||
		!strings.Contains(err.Error(), "hour") {
		t.Errorf("404: %v", err)
	}
	_, err = ParseConversion(http.StatusUnprocessableEntity, []byte(`{"message":"Validation Failed"}`))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 422 || !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("422: %v", err)
	}
	if _, err := ParseConversion(http.StatusCreated, []byte(`{"id": 42, "slug": "x"}`)); err == nil ||
		!strings.Contains(err.Error(), "client_id, html_url, pem") {
		t.Errorf("incomplete: %v", err)
	}
	if _, err := ParseConversion(http.StatusCreated, []byte(`not json`)); err == nil {
		t.Error("not JSON accepted")
	}
}

// observed is a configuration whose every check passes, on an organisation.
func observed() Observed {
	inst := &Installation{ID: 7, Permissions: map[string]string{"organization_self_hosted_runners": "write",
		"metadata": "read", "members": "read"}}
	inst.Account.Login = "acme"
	return Observed{
		Target:                 Target{Kind: Organization, Owner: "acme"},
		ConfiguredInstallation: 7,
		RunnerGroup:            "default",
		App:                    &AppInfo{ID: 42, Slug: "acme-runners", HTMLURL: "https://github.com/apps/acme-runners"},
		Installation:           inst,
		Groups:                 []RunnerGroup{{ID: 1, Name: "Default"}, {ID: 3, Name: "ci"}},
	}
}

func statuses(r Report) string {
	var s []string
	for _, c := range r.Checks {
		s = append(s, c.Name+"="+string(c.Status))
	}
	return strings.Join(s, " ")
}

func TestEvaluate(t *testing.T) {
	unauthorized := &StatusError{What: "signing in as the App", Status: http.StatusUnauthorized}
	for _, c := range []struct {
		name   string
		change func(*Observed)
		want   string
		detail string
	}{
		{"all pass", func(*Observed) {}, "key=pass installation=pass permissions=pass runnerGroup=pass", ""},
		{"key refused", func(o *Observed) {
			o.App, o.AppErr, o.Installation, o.Groups = nil, unauthorized, nil, nil
		}, "key=fail installation=skipped permissions=skipped runnerGroup=skipped", "clientID"},
		{"key not PEM", func(o *Observed) {
			o.App, o.AppErr, o.Installation, o.Groups = nil, errors.New("the app private key is not PEM"), nil, nil
		}, "key=fail installation=skipped permissions=skipped runnerGroup=skipped", "not PEM"},
		{"not installed", func(o *Observed) {
			o.Installation, o.Groups = nil, nil
			o.InstallationErr = &StatusError{Status: http.StatusNotFound}
		}, "key=pass installation=fail permissions=skipped runnerGroup=skipped",
			"https://github.com/apps/acme-runners/installations/new"},
		{"another installation configured", func(o *Observed) { o.ConfiguredInstallation = 8 },
			"key=pass installation=fail permissions=pass runnerGroup=pass", "installationID is 8"},
		{"read only", func(o *Observed) {
			o.Installation.Permissions["organization_self_hosted_runners"] = "read"
		}, "key=pass installation=pass permissions=fail runnerGroup=pass", "organization_self_hosted_runners: write (has read)"},
		{"no metadata", func(o *Observed) { delete(o.Installation.Permissions, "metadata") },
			"key=pass installation=pass permissions=fail runnerGroup=pass", "metadata: read (has none)"},
		{"groups forbidden", func(o *Observed) {
			o.Groups, o.GroupsErr = nil, &StatusError{Status: http.StatusForbidden}
		}, "key=pass installation=pass permissions=pass runnerGroup=fail", "organization_self_hosted_runners"},
		{"no such group", func(o *Observed) { o.RunnerGroup = "gpu" },
			"key=pass installation=pass permissions=pass runnerGroup=fail", "Default, ci"},
		{"named group", func(o *Observed) { o.RunnerGroup = "CI" },
			"key=pass installation=pass permissions=pass runnerGroup=pass", "ID 3"},
		{"enterprise", func(o *Observed) {
			o.Target = Target{Kind: Enterprise, Owner: "acme"}
			o.Installation, o.Groups = nil, nil
		}, "key=pass installation=fail permissions=skipped runnerGroup=skipped", "token"},
		{"repository", func(o *Observed) {
			o.Target = Target{Kind: Repository, Owner: "acme", Repo: "widgets"}
			o.Installation.Permissions = map[string]string{"administration": "write", "metadata": "read"}
			o.Groups = nil
		}, "key=pass installation=pass permissions=pass runnerGroup=pass", ""},
		{"repository with a group", func(o *Observed) {
			o.Target = Target{Kind: Repository, Owner: "acme", Repo: "widgets"}
			o.Installation.Permissions = map[string]string{"administration": "write", "metadata": "read"}
			o.Groups, o.RunnerGroup = nil, "ci"
		}, "key=pass installation=pass permissions=pass runnerGroup=fail", "set github.runnerGroup to default"},
		{"repository without administration", func(o *Observed) {
			o.Target = Target{Kind: Repository, Owner: "acme", Repo: "widgets"}
			o.Groups = nil
		}, "key=pass installation=pass permissions=fail runnerGroup=pass", "administration: write (has none)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := observed()
			c.change(&o)
			r := Evaluate(o)
			if got := statuses(r); got != c.want {
				t.Errorf("checks %s, want %s", got, c.want)
			}
			failure := r.Failure()
			if wantOK := !strings.Contains(c.want, "fail"); r.OK != wantOK || (failure == nil) != wantOK {
				t.Errorf("ok %v, failure %v", r.OK, failure)
			}
			if failure != nil {
				if e := op.AsError(failure); e.Code != op.CodeCheckFailed || !strings.Contains(e.Message, c.detail) {
					t.Errorf("failure %+v, want it to mention %q", e, c.detail)
				}
			}
			if c.detail != "" && failure == nil {
				var all []string
				for _, ch := range r.Checks {
					all = append(all, ch.Detail)
				}
				if !strings.Contains(strings.Join(all, "\n"), c.detail) {
					t.Errorf("details %q lack %q", all, c.detail)
				}
			}
		})
	}
}

func TestEvaluateReportsIDs(t *testing.T) {
	r := Evaluate(observed())
	if !r.OK || r.App != "acme-runners" || r.InstallationID != 7 || r.RunnerGroupID != 1 {
		t.Errorf("%+v", r)
	}
}

func TestFlowHandler(t *testing.T) {
	m, err := NewManifest("acme-runners", Organization, "http://127.0.0.1:4000/callback")
	if err != nil {
		t.Fatal(err)
	}
	codes := make(chan string, 1)
	h := flowHandler("127.0.0.1:4000", "s3cret", m,
		RegisterURL("https://github.com", "acme", "s3cret"), "acme", codes)
	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	page := get("127.0.0.1:4000", "/")
	body := page.Body.String()
	if page.Code != http.StatusOK ||
		!strings.Contains(body, `action="https://github.com/organizations/acme/settings/apps/new?state=s3cret"`) ||
		!strings.Contains(body, "organization_self_hosted_runners") {
		t.Fatalf("page %d:\n%s", page.Code, body)
	}
	// The manifest survives HTML escaping: the form posts exactly the JSON.
	start := strings.Index(body, `name="manifest" value="`) + len(`name="manifest" value="`)
	value := body[start : start+strings.Index(body[start:], `"`)]
	var posted Manifest
	unescaped := strings.NewReplacer("&#34;", `"`, "&quot;", `"`, "&amp;", "&", "&#39;", "'", "&lt;", "<",
		"&gt;", ">").Replace(value)
	if err := json.Unmarshal([]byte(unescaped), &posted); err != nil || posted.Name != "acme-runners" {
		t.Errorf("posted manifest %q: %v", unescaped, err)
	}

	if w := get("evil.example:4000", "/"); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("another host: %d", w.Code)
	}
	if w := get("127.0.0.1:4000", "/callback?code=c1&state=wrong"); w.Code != http.StatusBadRequest {
		t.Errorf("wrong state: %d", w.Code)
	}
	if w := get("127.0.0.1:4000", "/callback?state=s3cret"); w.Code != http.StatusBadRequest {
		t.Errorf("no code: %d", w.Code)
	}
	select {
	case c := <-codes:
		t.Fatalf("code %s delivered too early", c)
	default:
	}
	if w := get("127.0.0.1:4000", "/callback?code=c1&state="+url.QueryEscape("s3cret")); w.Code != http.StatusOK {
		t.Errorf("callback: %d", w.Code)
	}
	if w := get("127.0.0.1:4000", "/callback?code=c2&state=s3cret"); w.Code != http.StatusOK {
		t.Errorf("second callback: %d", w.Code)
	}
	if c := <-codes; c != "c1" {
		t.Errorf("code %s", c)
	}
	select {
	case c := <-codes:
		t.Errorf("a second code %s", c)
	default:
	}
}
