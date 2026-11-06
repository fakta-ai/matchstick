package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/fakta-ai/matchstick/internal/github"
)

// client calls GitHub's REST API.
type client struct {
	api  string
	http *http.Client
}

func newClient(api string) client {
	return client{api: api, http: &http.Client{Timeout: 30 * time.Second}}
}

// maxBody bounds what is read of an answer.
const maxBody = 1 << 20

// do sends a request and reads the answer into into, expecting status want.
func (c client) do(ctx context.Context, what, method, path, bearer string, want int, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.StatusCode != want {
		return statusError(what, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// convert exchanges a manifest code for the App it registered.
func (c client) convert(ctx context.Context, code string) (Conversion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.api+"/app-manifests/"+url.PathEscape(code)+"/conversions", bytes.NewReader(nil))
	if err != nil {
		return Conversion{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return Conversion{}, fmt.Errorf("exchanging the code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Conversion{}, fmt.Errorf("exchanging the code: %w", err)
	}
	return ParseConversion(resp.StatusCode, body)
}

// accountType returns whether a login is an "Organization" or a "User".
func (c client) accountType(ctx context.Context, login string) (string, error) {
	var u struct {
		Type string `json:"type"`
	}
	err := c.do(ctx, "looking up "+login, http.MethodGet, "/users/"+url.PathEscape(login), "", http.StatusOK, &u)
	return u.Type, err
}

// Gather reads, as the App signed in with creds, what the checks judge: the App, its installation on the target, and
// the target's runner groups. It stops at the first step that fails. It changes nothing on GitHub; reading runner
// groups takes a short-lived installation token, as run does.
func Gather(ctx context.Context, t Target, creds github.Credentials, runnerGroup string, now time.Time) Observed {
	o := Observed{Target: t, ConfiguredInstallation: creds.AppInstallationID, RunnerGroup: runnerGroup}
	c := newClient(t.API)
	jwt, err := github.AppJWT(creds, now)
	if err != nil {
		o.AppErr = err
		return o
	}
	var app AppInfo
	if o.AppErr = c.do(ctx, "signing in as the App", http.MethodGet, "/app", jwt, http.StatusOK, &app); o.AppErr != nil {
		return o
	}
	o.App = &app
	var path string
	switch t.Kind {
	case Organization:
		path = "/orgs/" + url.PathEscape(t.Owner) + "/installation"
	case Repository:
		path = "/repos/" + url.PathEscape(t.Owner) + "/" + url.PathEscape(t.Repo) + "/installation"
	default:
		return o
	}
	var inst Installation
	what := "finding the App's installation on " + t.Name()
	if o.InstallationErr = c.do(ctx, what, http.MethodGet, path, jwt, http.StatusOK, &inst); o.InstallationErr != nil {
		return o
	}
	o.Installation = &inst
	if t.Kind != Organization {
		return o
	}
	var token struct {
		Token string `json:"token"`
	}
	tokenPath := "/app/installations/" + strconv.FormatInt(inst.ID, 10) + "/access_tokens"
	if err := c.do(ctx, "signing in as the installation", http.MethodPost, tokenPath, jwt, http.StatusCreated,
		&token); err != nil {
		o.GroupsErr = err
		return o
	}
	o.Groups, o.GroupsErr = c.runnerGroups(ctx, token.Token, t.Owner)
	return o
}

// runnerGroups lists an organisation's runner groups, following its pages.
func (c client) runnerGroups(ctx context.Context, token, org string) ([]RunnerGroup, error) {
	all := []RunnerGroup{}
	for page := 1; ; page++ {
		var body struct {
			TotalCount   int           `json:"total_count"`
			RunnerGroups []RunnerGroup `json:"runner_groups"`
		}
		path := "/orgs/" + url.PathEscape(org) + "/actions/runner-groups?per_page=100&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "listing "+org+"'s runner groups", http.MethodGet, path, token, http.StatusOK,
			&body); err != nil {
			return nil, err
		}
		all = append(all, body.RunnerGroups...)
		if len(body.RunnerGroups) < 100 || len(all) >= body.TotalCount {
			return all, nil
		}
	}
}
