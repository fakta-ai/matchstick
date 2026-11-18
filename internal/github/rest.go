package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// restClient reads GitHub's REST runner list and runner groups, which the scale-set client does not offer.
type restClient struct {
	api         string // REST API root, such as https://api.github.com
	runnersPath string // such as /orgs/acme/actions/runners
	creds       Credentials
	http        *http.Client
	now         func() time.Time // the clock; time.Now outside tests

	mu      sync.Mutex
	token   string
	expires time.Time
}

// restRunner is an entry of GitHub's runner list.
type restRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

func newRESTClient(configURL string, c Credentials) (*restClient, error) {
	api, path, err := runnersEndpoint(configURL)
	if err != nil {
		return nil, err
	}
	return &restClient{api: api, runnersPath: path, creds: c, http: &http.Client{Timeout: 30 * time.Second},
		now: time.Now}, nil
}

// runnersEndpoint returns the REST API root and the runner list's path for an organisation, repository or enterprise
// URL on github.com or GitHub Enterprise Server.
func runnersEndpoint(configURL string) (api, path string, err error) {
	u, err := url.Parse(strings.TrimSuffix(configURL, "/"))
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("github: %q is not a GitHub URL", configURL)
	}
	api = u.Scheme + "://" + u.Host + "/api/v3"
	if u.Host == "github.com" || strings.HasSuffix(u.Host, ".ghe.com") {
		api = u.Scheme + "://api." + u.Host
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "enterprises":
		path = "/enterprises/" + parts[1] + "/actions/runners"
	case len(parts) == 1 && parts[0] != "":
		path = "/orgs/" + parts[0] + "/actions/runners"
	case len(parts) == 2:
		path = "/repos/" + parts[0] + "/" + parts[1] + "/actions/runners"
	default:
		return "", "", fmt.Errorf("github: %q is not an organisation, repository or enterprise URL", configURL)
	}
	return api, path, nil
}

// runners returns every runner in the list, following its pages.
func (c *restClient) runners(ctx context.Context) ([]restRunner, error) {
	var all []restRunner
	for page := 1; ; page++ {
		var body struct {
			TotalCount int          `json:"total_count"`
			Runners    []restRunner `json:"runners"`
		}
		if err := c.get(ctx, "listing runners", c.runnersPath+"?per_page=100&page="+strconv.Itoa(page),
			&body); err != nil {
			return nil, err
		}
		all = append(all, body.Runners...)
		if len(body.Runners) < 100 || len(all) >= body.TotalCount {
			return all, nil
		}
	}
}

// restGroup is an entry of GitHub's REST list of runner groups.
type restGroup struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// runnerGroups returns the organisation's or enterprise's runner groups, following their pages. A repository has
// none but its default group, so for a repository it returns none.
func (c *restClient) runnerGroups(ctx context.Context) ([]restGroup, error) {
	if strings.HasPrefix(c.runnersPath, "/repos/") {
		return nil, nil
	}
	path := strings.TrimSuffix(c.runnersPath, "/runners") + "/runner-groups"
	var all []restGroup
	for page := 1; ; page++ {
		var body struct {
			TotalCount   int         `json:"total_count"`
			RunnerGroups []restGroup `json:"runner_groups"`
		}
		if err := c.get(ctx, "listing runner groups", path+"?per_page=100&page="+strconv.Itoa(page),
			&body); err != nil {
			return nil, err
		}
		all = append(all, body.RunnerGroups...)
		if len(body.RunnerGroups) < 100 || len(all) >= body.TotalCount {
			return all, nil
		}
	}
}

func (c *restClient) get(ctx context.Context, what, path string, into any) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: %s: %s", what, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("github: %s: %w", what, err)
	}
	return nil
}

// bearer returns the token to send: the configured token, or a GitHub App installation token, renewed before it
// expires.
func (c *restClient) bearer(ctx context.Context) (string, error) {
	if c.creds.AppPrivateKey == "" {
		return c.creds.Token, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && c.expires.Sub(now) > 5*time.Minute {
		return c.token, nil
	}
	appJWT, err := appToken(c.creds, now)
	if err != nil {
		return "", err
	}
	path := "/app/installations/" + strconv.FormatInt(c.creds.AppInstallationID, 10) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github: installation token: %s", resp.Status)
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("github: installation token: %w", err)
	}
	c.token, c.expires = body.Token, body.ExpiresAt
	return c.token, nil
}

// appToken signs the short-lived JWT a GitHub App authenticates with.
func appToken(c Credentials, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(c.AppPrivateKey))
	if block == nil {
		return "", errors.New("github: the app private key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err8 != nil {
			return "", fmt.Errorf("github: the app private key: %w", err)
		}
		var ok bool
		if key, ok = parsed.(*rsa.PrivateKey); !ok {
			return "", errors.New("github: the app private key is not an RSA key")
		}
	}
	claims := jwt.RegisteredClaims{
		Issuer:    c.AppClientID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}
