package main

import (
	"context"
	"os"
	"time"

	"github.com/fakta-ai/matchstick/internal/github"
	"github.com/fakta-ai/matchstick/internal/githubapp"
	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/plan"
)

// AppCreateInput is github-app create's input.
type AppCreateInput struct {
	Org       string        `json:"org,omitempty" jsonschema:"the organisation whose runners the App registers; give org or repo"`
	Repo      string        `json:"repo,omitempty" jsonschema:"the repository, owner/name, whose runners the App registers; give org or repo"`
	Name      string        `json:"name" jsonschema:"the App's name, unique on the GitHub host, at most 34 characters"`
	Host      string        `json:"host,omitempty" jsonschema:"the GitHub host: https://github.com (the default), https://<name>.ghe.com, or a GitHub Enterprise Server's URL"`
	KeyFile   string        `json:"keyFile" jsonschema:"a new file for the App's private key, written with mode 0600"`
	NoBrowser bool          `json:"noBrowser,omitempty" jsonschema:"do not open a browser; only print the page's address on stderr"`
	Timeout   plan.Duration `json:"timeout,omitempty" jsonschema:"how long to wait for GitHub's answer, at most 1h (default 15m)"`
}

// AppCheckInput is github-app check's input.
type AppCheckInput struct {
	Config     string `json:"config,omitempty" jsonschema:"the configuration file (default matchstick.yaml)"`
	SecretsDir string `json:"secretsDir,omitempty" jsonschema:"the secret mount holding github-app-private-key (default /var/run/secrets/matchstick)"`
}

const (
	defaultHost          = "https://github.com"
	defaultCreateTimeout = 15 * time.Minute
)

func githubAppOperations() []*op.Operation {
	return []*op.Operation{
		op.Define("github-app create",
			"Create the GitHub App matchstick signs in as, through GitHub's App manifest flow. matchstick serves a "+
				"page on a loopback address, opens it in the browser and prints its address on stderr; the page "+
				"sends the App's manifest to GitHub, where you create the App, and GitHub sends the browser back "+
				"with a code that matchstick exchanges for the App. The App's private key goes only to keyFile, "+
				"mode 0600; the output gives that path. The App is private, has no webhook, and has only the "+
				"permissions matchstick needs: on an organisation, Self-hosted runners: write; on a repository, "+
				"Administration: write; Metadata: read on both. GitHub Apps cannot register enterprise runners: an "+
				"enterprise needs a token. Install the App at installURL afterwards.",
			"matchstick github-app create --org acme --name acme-runners --key-file secrets/github-app-private-key",
			op.Annotations{OpenWorld: true},
			createApp),
		op.Define("github-app check",
			"Check the GitHub App that run signs in as, with the same configuration and secret mount. It checks "+
				"that GitHub accepts a JWT signed with the key (key); the App is installed on github.url's organisation or "+
				"repository, with the installation ID in the configuration (installation); the installation has the "+
				"permissions matchstick needs (permissions); github.runnerGroup exists and the App can use it "+
				"(runnerGroup). One entry per check; exits 1 when any check fails. Changes nothing on GitHub.",
			"matchstick github-app check --config matchstick.yaml --secrets-dir secrets",
			op.Annotations{ReadOnly: true, Idempotent: true, OpenWorld: true},
			checkApp),
	}
}

func createApp(ctx context.Context, in AppCreateInput) (githubapp.Created, error) {
	host := in.Host
	if host == "" {
		host = defaultHost
	}
	t, err := githubapp.NewTarget(host, in.Org, in.Repo)
	if err != nil {
		return githubapp.Created{}, op.Errorf(op.CodeInvalidInput, "%v", err)
	}
	timeout := time.Duration(in.Timeout)
	if timeout == 0 {
		timeout = defaultCreateTimeout
	}
	return githubapp.Create(ctx, githubapp.CreateOptions{Target: t, Name: in.Name, KeyFile: in.KeyFile,
		Browser: !in.NoBrowser, Timeout: timeout, Prompt: os.Stderr})
}

func checkApp(ctx context.Context, in AppCheckInput) (githubapp.Report, error) {
	cfg, err := loadConfig(in.Config)
	if err != nil {
		return githubapp.Report{}, err
	}
	if cfg.GitHub == nil {
		return githubapp.Report{}, op.Errorf(op.CodeInvalidConfig, "the configuration has no github section")
	}
	if cfg.GitHub.App == nil {
		return githubapp.Report{}, op.Errorf(op.CodeInvalidConfig,
			"github.app is not configured, so matchstick signs in with a token; github-app check checks a GitHub App")
	}
	t, err := githubapp.ParseURL(cfg.GitHub.URL)
	if err != nil {
		return githubapp.Report{}, op.Errorf(op.CodeInvalidConfig, "%v", err)
	}
	creds, err := secretSource{dir: in.SecretsDir}.read()
	if err != nil {
		return githubapp.Report{}, op.Errorf(op.CodeInvalidConfig, "%v", err)
	}
	if creds.AppPrivateKey == "" {
		return githubapp.Report{}, op.Errorf(op.CodeInvalidConfig,
			"the secret mount holds a token, not the App's key (github-app-private-key)")
	}
	observed := githubapp.Gather(ctx, t, github.Credentials{AppClientID: cfg.GitHub.App.ClientID,
		AppInstallationID: cfg.GitHub.App.InstallationID, AppPrivateKey: creds.AppPrivateKey},
		cfg.GitHub.RunnerGroup, now())
	return githubapp.Evaluate(observed), nil
}
