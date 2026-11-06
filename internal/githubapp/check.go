package githubapp

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/fakta-ai/matchstick/internal/op"
)

// Status is a check's result.
type Status string

// Statuses.
const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Skipped Status = "skipped"
)

// Check names, in the order they run.
const (
	CheckKey          = "key"
	CheckInstallation = "installation"
	CheckPermissions  = "permissions"
	CheckRunnerGroup  = "runnerGroup"
)

// Check is one check's result.
type Check struct {
	Name   string `json:"name" jsonschema:"key, installation, permissions or runnerGroup"`
	Status Status `json:"status" jsonschema:"pass, fail, or skipped when an earlier check's failure leaves nothing to check"`
	Detail string `json:"detail" jsonschema:"what was found, and on failure what to change"`
}

// Report is the result of checking a GitHub App against a configuration.
type Report struct {
	OK bool `json:"ok" jsonschema:"no check failed"`
	// App is the App's slug, once its key is accepted.
	App string `json:"app,omitempty" jsonschema:"the App's slug, once GitHub accepts its key"`
	// InstallationID is the App's installation on the configured owner, once found.
	InstallationID int64 `json:"installationID,omitempty" jsonschema:"the App's installation on the configured organisation or repository, once found"`
	// RunnerGroupID is the configured runner group's ID, once found.
	RunnerGroupID int64   `json:"runnerGroupID,omitempty" jsonschema:"the configured runner group's ID, on an organisation, once found"`
	Checks        []Check `json:"checks" jsonschema:"one entry per check: key, installation, permissions, runnerGroup"`
}

// Failure reports the failed checks, if any.
func (r Report) Failure() error {
	var failed []string
	for _, c := range r.Checks {
		if c.Status == Fail {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return op.Errorf(op.CodeCheckFailed, "github-app check failed: %s", strings.Join(failed, "; "))
}

// AppInfo is the App as GET /app reports it.
type AppInfo struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
}

// Installation is an installation of the App, as GET /orgs/{org}/installation or
// /repos/{owner}/{repo}/installation reports it.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	Permissions map[string]string `json:"permissions"`
}

// RunnerGroup is an organisation's runner group, as GET /orgs/{org}/actions/runner-groups lists it.
type RunnerGroup struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// Observed is what GitHub reported about the App, read in the order of the checks. A step not taken leaves its fields
// zero.
type Observed struct {
	Target Target
	// ConfiguredInstallation is the configuration's github.app.installationID.
	ConfiguredInstallation int64
	// RunnerGroup is the configuration's github.runnerGroup.
	RunnerGroup string

	// App, or AppErr: GET /app, signed with the App's JWT.
	App    *AppInfo
	AppErr error
	// Installation, or InstallationErr: the App's installation on the target.
	Installation    *Installation
	InstallationErr error
	// Groups, or GroupsErr: the organisation's runner groups, read with an installation token. Groups is nil when
	// they were not read.
	Groups    []RunnerGroup
	GroupsErr error
}

// Evaluate judges what was observed: one entry per check, in order.
func Evaluate(o Observed) Report {
	r := Report{Checks: []Check{key(o), installation(o)}}
	r.Checks = append(r.Checks, permissions(o, r.Checks[1]), runnerGroup(o, r.Checks[1]))
	if o.App != nil {
		r.App = o.App.Slug
	}
	if o.Installation != nil {
		r.InstallationID = o.Installation.ID
	}
	if g := findGroup(o.Groups, o.RunnerGroup); g != nil && o.Target.Kind == Organization {
		r.RunnerGroupID = g.ID
	}
	r.OK = !slices.ContainsFunc(r.Checks, func(c Check) bool { return c.Status == Fail })
	return r
}

func key(o Observed) Check {
	c := Check{Name: CheckKey}
	switch {
	case o.App != nil:
		c.Status, c.Detail = Pass, fmt.Sprintf("GitHub accepts the key's JWT: App %s (ID %d)", o.App.Slug, o.App.ID)
	case hasStatus(o.AppErr, http.StatusUnauthorized):
		c.Status, c.Detail = Fail, fmt.Sprintf("GitHub refused the JWT (%v): the private key is not this App's, "+
			"or github.app.clientID is not its client ID", o.AppErr)
	case o.AppErr != nil:
		c.Status, c.Detail = Fail, o.AppErr.Error()
	default:
		c.Status, c.Detail = Skipped, "not checked"
	}
	return c
}

func installation(o Observed) Check {
	c := Check{Name: CheckInstallation}
	switch {
	case o.Target.Kind == Enterprise:
		c.Status, c.Detail = Fail, "GitHub Apps cannot register enterprise runners; sign in to an enterprise with a "+
			"token, or point github.url at an organisation"
	case o.App == nil:
		c.Status, c.Detail = Skipped, "not checked: GitHub did not accept the key"
	case hasStatus(o.InstallationErr, http.StatusNotFound):
		c.Status, c.Detail = Fail, fmt.Sprintf("App %s is not installed on %s; install it at %s", o.App.Slug,
			o.Target.Name(), InstallURL(o.App.HTMLURL))
	case o.InstallationErr != nil:
		c.Status, c.Detail = Fail, o.InstallationErr.Error()
	case o.Installation == nil:
		c.Status, c.Detail = Skipped, "not checked"
	case o.ConfiguredInstallation != o.Installation.ID:
		c.Status, c.Detail = Fail, fmt.Sprintf("App %s is installed on %s as installation %d, but "+
			"github.app.installationID is %d", o.App.Slug, o.Target.Name(), o.Installation.ID, o.ConfiguredInstallation)
	default:
		c.Status, c.Detail = Pass, fmt.Sprintf("App %s is installed on %s as installation %d", o.App.Slug,
			o.Target.Name(), o.Installation.ID)
	}
	return c
}

// permissions compares the installation's permissions with what matchstick needs. A mismatched installation ID
// still has its permissions checked: they are the installation's that GitHub reported.
func permissions(o Observed, inst Check) Check {
	c := Check{Name: CheckPermissions}
	if o.Installation == nil {
		c.Status, c.Detail = Skipped, "not checked: "+inst.Name+" "+string(inst.Status)
		return c
	}
	need := Permissions(o.Target.Kind)
	var have, missing []string
	for _, name := range sortedKeys(need) {
		got := o.Installation.Permissions[name]
		if covers(got, need[name]) {
			have = append(have, name+": "+got)
			continue
		}
		if got == "" {
			got = "none"
		}
		missing = append(missing, fmt.Sprintf("%s: %s (has %s)", name, need[name], got))
	}
	if len(missing) > 0 {
		c.Status, c.Detail = Fail, "the installation lacks "+strings.Join(missing, ", ")+"; add them in the App's "+
			"settings, then accept the new permissions on the installation"
		return c
	}
	c.Status, c.Detail = Pass, "the installation has "+strings.Join(have, ", ")
	return c
}

func runnerGroup(o Observed, inst Check) Check {
	c := Check{Name: CheckRunnerGroup}
	switch {
	case o.Target.Kind == Repository && strings.EqualFold(o.RunnerGroup, "default"):
		c.Status, c.Detail = Pass, "a repository's runners belong to its default runner group"
	case o.Target.Kind == Repository:
		c.Status, c.Detail = Fail, fmt.Sprintf("runner groups belong to organisations; a repository's runners "+
			"belong to its default group, so set github.runnerGroup to default, not %q", o.RunnerGroup)
	case o.Target.Kind != Organization:
		c.Status, c.Detail = Skipped, "not checked: "+inst.Name+" "+string(inst.Status)
	case hasStatus(o.GroupsErr, http.StatusForbidden) || hasStatus(o.GroupsErr, http.StatusNotFound):
		c.Status, c.Detail = Fail, fmt.Sprintf("the installation cannot read %s's runner groups (%v): it needs "+
			"organization_self_hosted_runners", o.Target.Owner, o.GroupsErr)
	case o.GroupsErr != nil:
		c.Status, c.Detail = Fail, o.GroupsErr.Error()
	case o.Groups == nil:
		c.Status, c.Detail = Skipped, "not checked: "+inst.Name+" "+string(inst.Status)
	default:
		g := findGroup(o.Groups, o.RunnerGroup)
		if g == nil {
			names := make([]string, 0, len(o.Groups))
			for _, g := range o.Groups {
				names = append(names, g.Name)
			}
			c.Status, c.Detail = Fail, fmt.Sprintf("%s has no runner group %q that the App can use; it can use: %s",
				o.Target.Owner, o.RunnerGroup, strings.Join(names, ", "))
			break
		}
		c.Status, c.Detail = Pass, fmt.Sprintf("runner group %s (ID %d) is one the App can use", g.Name, g.ID)
	}
	return c
}

// findGroup finds a runner group by name, ignoring case as GitHub does ("default" is the Default group).
func findGroup(groups []RunnerGroup, name string) *RunnerGroup {
	for i := range groups {
		if strings.EqualFold(groups[i].Name, name) {
			return &groups[i]
		}
	}
	return nil
}

// covers reports whether access level got includes need: write includes read.
func covers(got, need string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return got != "" && rank[got] >= rank[need]
}

func hasStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
