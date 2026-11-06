// Package githubapp creates the GitHub App matchstick signs in as, through GitHub's App manifest flow, and checks an
// existing one: its key, its installation, the installation's permissions and the runner group. The decisions (the
// manifest, reading GitHub's answers, judging a check) are pure functions; the HTTP calls around them are thin.
package githubapp

import (
	"fmt"
	"net/url"
	"strings"
)

// Kind is what runners register with.
type Kind string

// Kinds.
const (
	Organization Kind = "organization"
	Repository   Kind = "repository"
	Enterprise   Kind = "enterprise"
)

// Target is where runners register: an organisation, a repository or an enterprise, on one GitHub host.
type Target struct {
	Kind Kind
	// Owner is the organisation, the repository's owner or the enterprise.
	Owner string
	// Repo is the repository's name, for Repository.
	Repo string
	// Web is the host's web root, such as https://github.com.
	Web string
	// API is the host's REST API root, such as https://api.github.com.
	API string
}

// Name returns the target as GitHub writes it: acme, or acme/widgets for a repository.
func (t Target) Name() string {
	if t.Kind == Repository {
		return t.Owner + "/" + t.Repo
	}
	return t.Owner
}

// Host returns the web and REST API roots of a GitHub host: https://github.com, a GitHub Enterprise Cloud host with
// data residency (https://acme.ghe.com, API at https://api.acme.ghe.com) or a GitHub Enterprise Server
// (https://github.acme.com, API at https://github.acme.com/api/v3).
func Host(base string) (web, api string, err error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("%q is not a GitHub host URL such as https://github.com", base)
	}
	if u.Path != "" {
		return "", "", fmt.Errorf("%q: give the host only, such as https://github.com, without a path", base)
	}
	web, api = hostRoots(u)
	return web, api, nil
}

func hostRoots(u *url.URL) (web, api string) {
	web = u.Scheme + "://" + u.Host
	if u.Host == "github.com" || strings.HasSuffix(u.Host, ".ghe.com") {
		return web, u.Scheme + "://api." + u.Host
	}
	return web, web + "/api/v3"
}

// ParseURL reads a configuration's github.url: https://<host>/<org>, https://<host>/<owner>/<repo> or
// https://<host>/enterprises/<enterprise>.
func ParseURL(s string) (Target, error) {
	u, err := url.Parse(strings.TrimSuffix(s, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Target{}, fmt.Errorf("github.url %q is not a GitHub URL", s)
	}
	web, api := hostRoots(u)
	t := Target{Web: web, API: api}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "enterprises" && parts[1] != "":
		t.Kind, t.Owner = Enterprise, parts[1]
	case len(parts) == 1 && parts[0] != "":
		t.Kind, t.Owner = Organization, parts[0]
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		t.Kind, t.Owner, t.Repo = Repository, parts[0], parts[1]
	default:
		return Target{}, fmt.Errorf("github.url %q is not an organisation, repository or enterprise URL", s)
	}
	return t, nil
}

// NewTarget returns the target of an organisation or a repository (owner/name) on the GitHub host at base. Exactly one
// of org and repo is given.
func NewTarget(base, org, repo string) (Target, error) {
	web, api, err := Host(base)
	if err != nil {
		return Target{}, err
	}
	t := Target{Web: web, API: api}
	switch {
	case (org == "") == (repo == ""):
		return Target{}, fmt.Errorf("give either an organisation (org) or a repository (repo), not both or neither")
	case org != "":
		if strings.Contains(org, "/") {
			return Target{}, fmt.Errorf("org %q: an organisation's name has no slash; a repository goes in repo", org)
		}
		t.Kind, t.Owner = Organization, org
	default:
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return Target{}, fmt.Errorf("repo %q: want owner/name, such as acme/widgets", repo)
		}
		t.Kind, t.Owner, t.Repo = Repository, owner, name
	}
	return t, nil
}
