// Package config reads and checks matchstick's configuration file. The file holds no secrets.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/fakta-ai/matchstick/internal/naming"
	"github.com/fakta-ai/matchstick/internal/plan"
)

// Name length limits: machine and runner names (<instance>-<pool>-<11 characters>) stay well within the 64
// characters VM names, host names and runner names allow.
const (
	maxInstanceLength = 10
	maxPoolLength     = 20
)

// Config is the whole configuration.
type Config struct {
	// Instance names this installation. It starts every machine and runner name and marks every machine as ours, so
	// it must be unique among installations sharing a GitHub organisation or a cloud resource group.
	Instance string `json:"instance"`
	// Interval is the longest time between two rounds of a pool.
	Interval plan.Duration `json:"interval,omitempty"`
	Listen   Listen        `json:"listen,omitzero"`
	// ClaimURL is the address machines post their claim to: https://<host>[:<port>][/<path>]. The host must reach
	// listen.claim from the machines' network. Required with azure.
	ClaimURL string  `json:"claimURL,omitempty"`
	GitHub   *GitHub `json:"github,omitempty"`
	Azure    *Azure  `json:"azure,omitempty"`
	// Fake replaces GitHub and the cloud with pools held in this file, to try the planner.
	Fake  *Fake  `json:"fake,omitempty"`
	Pools []Pool `json:"pools"`
}

// Listen holds the addresses matchstick serves.
type Listen struct {
	// Metrics serves /metrics and /healthz.
	Metrics string `json:"metrics,omitempty"`
	// Admin serves the drain endpoint; keep it on a loopback address.
	Admin string `json:"admin,omitempty"`
	// Claim serves the claim endpoint, over TLS, when ClaimURL is set.
	Claim string `json:"claim,omitempty"`
}

// GitHub says where the pools' scale sets live and how to sign in. The credential itself is a secret, read
// separately.
type GitHub struct {
	// URL is the organisation, repository or enterprise the scale sets belong to, such as https://github.com/acme.
	URL string `json:"url"`
	// RunnerGroup is the runner group of the scale sets of pools that name none.
	RunnerGroup string `json:"runnerGroup,omitempty"`
	// App identifies the GitHub App to sign in as; without it, matchstick signs in with a token.
	App *App `json:"app,omitempty"`
}

// App identifies a GitHub App installation.
type App struct {
	ClientID       string `json:"clientID"`
	InstallationID int64  `json:"installationID"`
}

// Azure says where the machines go.
type Azure struct {
	SubscriptionID string `json:"subscriptionID"`
	ResourceGroup  string `json:"resourceGroup"`
	Location       string `json:"location"`
	// SubnetID is the subnet the machines' network interfaces join; machines get no public address.
	SubnetID string `json:"subnetID"`
}

// Pool is one pool: a scale set and the machines serving it.
type Pool struct {
	Name string `json:"name"`
	// Labels are the scale set's labels, which workflows ask for in runs-on; the pool's name if empty.
	Labels []string `json:"labels,omitempty"`
	// RunnerGroup is the runner group of the pool's scale set; github.runnerGroup if empty.
	RunnerGroup string `json:"runnerGroup,omitempty"`
	// Maximum is the most machines the pool has at once, in any state.
	Maximum int `json:"maximum"`
	// Spares is how many idle machines to keep ready beyond demand.
	Spares int `json:"spares,omitempty"`
	// MaxLifetime is how long a machine may live, job or not.
	MaxLifetime plan.Duration `json:"maxLifetime,omitempty"`
	// RegistrationTimeout is how long a machine may run before its runner registers.
	RegistrationTimeout plan.Duration `json:"registrationTimeout,omitempty"`
	Azure               *AzurePool    `json:"azure,omitempty"`
}

// AzurePool is a pool's machine shape on Azure.
type AzurePool struct {
	// Size is the VM size, such as Standard_D4ads_v5.
	Size string `json:"size"`
	// Image is a Compute Gallery image version's resource ID, or an image definition's for its latest version.
	Image string `json:"image"`
	// Identity is a user-assigned managed identity's resource ID; empty for none.
	Identity string `json:"identity,omitempty"`
	// EphemeralOSDisk puts the OS disk on the VM's local storage: "ResourceDisk", "CacheDisk" or "NvmeDisk".
	// Empty for a managed disk.
	EphemeralOSDisk string `json:"ephemeralOSDisk,omitempty"`
	// OSDiskSizeGB sets the OS disk's size; 0 for the image's.
	OSDiskSizeGB int32 `json:"osDiskSizeGB,omitempty"`
	// Spot asks for Spot priority, falling back to regular priority when Spot has no capacity.
	Spot bool `json:"spot,omitempty"`
	// AcceleratedNetworking enables it on the network interface.
	AcceleratedNetworking bool `json:"acceleratedNetworking,omitempty"`
	// CustomData is a file whose contents become the VM's custom data, such as a cloud-init configuration.
	CustomData string `json:"customData,omitempty"`
}

// Fake holds made-up pools for trying the planner without GitHub or a cloud.
type Fake struct {
	Pools map[string]FakePool `json:"pools"`
}

// FakePool is what a fake pool reports.
type FakePool struct {
	Demand   *int           `json:"demand,omitempty"`
	Machines []plan.Machine `json:"machines,omitempty"`
	Runners  []plan.Runner  `json:"runners,omitempty"`
}

// Defaults.
const (
	DefaultInterval            = 30 * time.Second
	DefaultMaxLifetime         = 6 * time.Hour
	DefaultRegistrationTimeout = 10 * time.Minute
	DefaultMetrics             = ":9090"
	DefaultAdmin               = "127.0.0.1:9091"
	DefaultClaim               = ":8443"
	DefaultRunnerGroup         = "default"
)

// Load reads, defaults and checks a configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse defaults and checks a configuration given as YAML or JSON. Unknown fields are errors.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = plan.Duration(DefaultInterval)
	}
	if c.Listen.Metrics == "" {
		c.Listen.Metrics = DefaultMetrics
	}
	if c.Listen.Admin == "" {
		c.Listen.Admin = DefaultAdmin
	}
	if c.Listen.Claim == "" {
		c.Listen.Claim = DefaultClaim
	}
	if c.GitHub != nil && c.GitHub.RunnerGroup == "" {
		c.GitHub.RunnerGroup = DefaultRunnerGroup
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if len(p.Labels) == 0 {
			p.Labels = []string{p.Name}
		}
		if c.GitHub != nil && p.RunnerGroup == "" {
			p.RunnerGroup = c.GitHub.RunnerGroup
		}
		if p.MaxLifetime == 0 {
			p.MaxLifetime = plan.Duration(DefaultMaxLifetime)
		}
		if p.RegistrationTimeout == 0 {
			p.RegistrationTimeout = plan.Duration(DefaultRegistrationTimeout)
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(naming.ValidInstance(c.Instance, maxInstanceLength),
		"instance %q: lower-case letters and digits, starting with a letter, at most %d characters",
		c.Instance, maxInstanceLength)
	check(c.Interval > 0, "interval must be positive")
	for _, addr := range []string{c.Listen.Metrics, c.Listen.Admin, c.Listen.Claim} {
		_, _, err := net.SplitHostPort(addr)
		check(err == nil, "listen address %q: %v", addr, err)
	}
	if c.Fake != nil {
		check(c.GitHub == nil && c.Azure == nil, "fake replaces github and azure; set one or the other")
	} else {
		check(c.GitHub != nil, "github is required")
		check(c.Azure != nil, "azure is required")
	}
	if c.GitHub != nil {
		check(c.GitHub.URL != "", "github.url is required")
		if c.GitHub.App != nil {
			check(c.GitHub.App.ClientID != "" && c.GitHub.App.InstallationID > 0,
				"github.app needs clientID and installationID")
		}
	}
	if c.Azure != nil {
		a := c.Azure
		check(a.SubscriptionID != "" && a.ResourceGroup != "" && a.Location != "" && a.SubnetID != "",
			"azure needs subscriptionID, resourceGroup, location and subnetID")
		check(c.ClaimURL != "", "claimURL is required with azure: machines claim their runner configuration there")
	}
	if c.ClaimURL != "" {
		u, err := url.Parse(c.ClaimURL)
		check(err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" &&
			u.Fragment == "", "claimURL %q: want https://<host>[:<port>][/<path>]", c.ClaimURL)
	}
	check(len(c.Pools) > 0, "at least one pool is required")
	seen := map[string]bool{}
	for _, p := range c.Pools {
		check(naming.ValidPool(p.Name, maxPoolLength),
			"pool %q: lower-case letters, digits and inner hyphens, starting with a letter, at most %d characters",
			p.Name, maxPoolLength)
		check(p.RunnerGroup == "" || c.GitHub != nil, "pool %s: runnerGroup needs github", p.Name)
		check(!seen[p.Name], "pool %q appears twice", p.Name)
		seen[p.Name] = true
		check(p.Maximum >= 0, "pool %s: maximum must not be negative", p.Name)
		check(p.Spares >= 0 && p.Spares <= p.Maximum, "pool %s: spares must be between 0 and maximum", p.Name)
		check(p.RegistrationTimeout > 0, "pool %s: registrationTimeout must be positive", p.Name)
		check(p.MaxLifetime > p.RegistrationTimeout, "pool %s: maxLifetime must exceed registrationTimeout", p.Name)
		if c.Azure != nil {
			check(p.Azure != nil, "pool %s: azure is required", p.Name)
		}
		if p.Azure != nil {
			check(p.Azure.Size != "" && p.Azure.Image != "", "pool %s: azure needs size and image", p.Name)
			switch p.Azure.EphemeralOSDisk {
			case "", "ResourceDisk", "CacheDisk", "NvmeDisk":
			default:
				check(false, "pool %s: azure.ephemeralOSDisk must be ResourceDisk, CacheDisk, NvmeDisk or empty",
					p.Name)
			}
			check(p.Azure.OSDiskSizeGB >= 0, "pool %s: azure.osDiskSizeGB must not be negative", p.Name)
		}
		if c.Fake != nil {
			_, ok := c.Fake.Pools[p.Name]
			check(ok, "pool %s: fake.pools has no entry for it", p.Name)
		}
	}
	return errors.Join(errs...)
}

// Limits returns the planner's limits for a pool.
func (p Pool) Limits() plan.Limits {
	return plan.Limits{Maximum: p.Maximum, Spares: p.Spares, RegistrationTimeout: p.RegistrationTimeout}
}
