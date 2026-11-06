package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// homepage is the App's homepage in its manifest; GitHub requires one.
const homepage = "https://github.com/fakta-ai/matchstick"

// maxNameLength is the longest name GitHub gives an App.
const maxNameLength = 34

// Permissions returns the permissions matchstick needs on a target, as GitHub names them: on an organisation,
// organization_self_hosted_runners: write; on a repository, administration: write; on both, metadata: read. Runners of
// an enterprise cannot be registered by a GitHub App; nil for Enterprise.
func Permissions(k Kind) map[string]string {
	switch k {
	case Organization:
		return map[string]string{"organization_self_hosted_runners": "write", "metadata": "read"}
	case Repository:
		return map[string]string{"administration": "write", "metadata": "read"}
	default:
		return nil
	}
}

// Manifest is a GitHub App manifest: the App GitHub registers when the manifest is posted to it.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// NewManifest returns the manifest of a private App named name with the permissions matchstick needs on a target of
// kind k, no webhook and no events. GitHub sends the browser back to redirectURL with the code to exchange.
func NewManifest(name string, k Kind, redirectURL string) (Manifest, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return Manifest{}, errors.New("the App needs a name")
	case len([]rune(name)) > maxNameLength:
		return Manifest{}, fmt.Errorf("the App's name %q is longer than GitHub's %d characters", name, maxNameLength)
	}
	permissions := Permissions(k)
	if permissions == nil {
		return Manifest{}, errors.New("GitHub Apps cannot register enterprise runners: use an organisation or a " +
			"repository, or sign in to an enterprise with a token")
	}
	return Manifest{
		Name:               name,
		URL:                homepage,
		Description:        "Registers ephemeral self-hosted GitHub Actions runners, one fresh virtual machine per job.",
		RedirectURL:        redirectURL,
		DefaultPermissions: permissions,
		DefaultEvents:      []string{},
	}, nil
}

// RegisterURL returns where the manifest is posted: the organisation's App settings, or the signed-in user's when
// organisation is empty. state comes back with the code.
func RegisterURL(web, organisation, state string) string {
	path := "/settings/apps/new"
	if organisation != "" {
		path = "/organizations/" + url.PathEscape(organisation) + "/settings/apps/new"
	}
	return web + path + "?state=" + url.QueryEscape(state)
}

// Conversion is GitHub's answer to a manifest code: the registered App and its private key.
type Conversion struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	HTMLURL  string `json:"html_url"`
	PEM      string `json:"pem"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// String hides the private key.
func (c Conversion) String() string {
	return fmt.Sprintf("githubapp.Conversion{ID: %d, Slug: %q}", c.ID, c.Slug)
}

// GoString hides the private key from %#v too.
func (c Conversion) GoString() string { return c.String() }

// ParseConversion reads GitHub's answer to POST /app-manifests/{code}/conversions.
func ParseConversion(status int, body []byte) (Conversion, error) {
	switch status {
	case http.StatusCreated:
	case http.StatusNotFound:
		return Conversion{}, errors.New("GitHub does not know this code: it was already used, or is more than an " +
			"hour old; run github-app create again")
	default:
		return Conversion{}, statusError("exchanging the code", status, body)
	}
	var c Conversion
	if err := json.Unmarshal(body, &c); err != nil {
		return Conversion{}, fmt.Errorf("exchanging the code: GitHub's answer is not the App: %w", err)
	}
	var missing []string
	for field, empty := range map[string]bool{"id": c.ID <= 0, "slug": c.Slug == "", "client_id": c.ClientID == "",
		"html_url": c.HTMLURL == "", "pem": !strings.Contains(c.PEM, "PRIVATE KEY-----")} {
		if empty {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Conversion{}, fmt.Errorf("exchanging the code: GitHub's answer lacks %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// InstallURL returns where the App is installed, from its page's URL.
func InstallURL(htmlURL string) string {
	return strings.TrimSuffix(htmlURL, "/") + "/installations/new"
}

// StatusError is GitHub answering with a status other than the one expected.
type StatusError struct {
	What    string
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s: GitHub answered %d %s", e.What, e.Status, http.StatusText(e.Status))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// statusError builds a StatusError with GitHub's message from body, if it has one.
func statusError(what string, status int, body []byte) *StatusError {
	var b struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &b)
	return &StatusError{What: what, Status: status, Message: b.Message}
}
