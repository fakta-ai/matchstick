package config

import (
	"strings"
	"testing"
	"time"
)

const azure = `
instance: ci
claimURL: https://matchstick.runners.example:8443/claim
github: {url: https://github.com/acme, app: {clientID: Iv1.abc, installationID: 7}}
azure: {subscriptionID: s, resourceGroup: g, location: swedencentral, subnetID: /subnets/x}
pools:
  - name: build
    maximum: 3
    azure: {size: Standard_D4ads_v5, image: /galleries/g/images/i/versions/1.0.0, ephemeralOSDisk: ResourceDisk}
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(azure))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pools[0]
	if time.Duration(c.Interval) != DefaultInterval || c.Listen.Admin != DefaultAdmin || c.Listen.Claim != DefaultClaim ||
		c.GitHub.RunnerGroup != DefaultRunnerGroup || p.Labels[0] != "build" ||
		time.Duration(p.MaxLifetime) != DefaultMaxLifetime ||
		time.Duration(p.RegistrationTimeout) != DefaultRegistrationTimeout {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"unknown field":      {"maximum: 3", "maximum: 3\n    colour: red", "unknown field"},
		"bad instance":       {"instance: ci", "instance: CI", "instance"},
		"hyphen in instance": {"instance: ci", "instance: c-i", "instance"},
		"long pool name":     {"name: build", "name: averyveryverylongpoolname", "pool"},
		"spares over max":    {"maximum: 3", "maximum: 3\n    spares: 4", "spares"},
		"lifetime too short": {"maximum: 3", "maximum: 3\n    maxLifetime: 5m", "maxLifetime"},
		"bad disk":           {"ResourceDisk", "Floppy", "ephemeralOSDisk"},
		"no github":          {"github: {url: https://github.com/acme, app: {clientID: Iv1.abc, installationID: 7}}", "", "github is required"},
		"duplicate pool": {"pools:\n", "pools:\n  - {name: build, maximum: 1, azure: {size: s, image: i}}\n",
			"appears twice"},
		"bad duration":          {"maximum: 3", "maximum: 3\n    maxLifetime: forever", "duration"},
		"no claimURL":           {"claimURL: https://matchstick.runners.example:8443/claim\n", "", "claimURL is required"},
		"http claimURL":         {"claimURL: https://", "claimURL: http://", "claimURL"},
		"claimURL with a query": {"8443/claim", "8443/claim?x=1", "claimURL"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(azure, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestFakeReplacesGitHubAndAzure(t *testing.T) {
	_, err := Parse([]byte("instance: ci\nfake: {pools: {build: {demand: 1}}}\npools: [{name: build, maximum: 2}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse([]byte("instance: ci\nfake: {pools: {}}\npools: [{name: build, maximum: 2}]\n"))
	if err == nil || !strings.Contains(err.Error(), "fake.pools") {
		t.Errorf("error = %v, want a missing fake pool", err)
	}
}
