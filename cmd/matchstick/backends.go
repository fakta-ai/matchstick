package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/fakta-ai/matchstick/internal/config"
	"github.com/fakta-ai/matchstick/internal/github"
	githubfake "github.com/fakta-ai/matchstick/internal/github/fake"
	"github.com/fakta-ai/matchstick/internal/op"
	"github.com/fakta-ai/matchstick/internal/provider"
	"github.com/fakta-ai/matchstick/internal/provider/azure"
	providerfake "github.com/fakta-ai/matchstick/internal/provider/fake"
	"github.com/fakta-ai/matchstick/internal/secret"
)

// backend is one pool's cloud and GitHub.
type backend struct {
	machines provider.Provider
	runners  github.Runners
	// demand reads the pool's demand once; nil if unknown.
	demand func(context.Context) (*int, error)
	// listen ensures the pool's scale set, then reports demand to the pool until ctx ends; nil for the fake.
	listen func(ctx context.Context, sink github.Sink)
	// runnerGroup is the runner group the pool's scale set was found in; empty for the fake and for run.
	runnerGroup string
}

// secretSource says where the GitHub credential comes from: standard input, or the secret mount directory.
type secretSource struct {
	stdin bool
	dir   string
}

func (s secretSource) read() (secret.GitHub, error) {
	if s.stdin {
		return secret.FromStdin(os.Stdin)
	}
	dir := s.dir
	if dir == "" {
		dir = secret.DefaultDir
	}
	return secret.FromDir(dir)
}

// newBackends builds each pool's backend. With find, it looks up each pool's scale set now, changing nothing, and
// fails if one is missing; without, each pool's listener ensures its scale set when it starts.
func newBackends(ctx context.Context, cfg *config.Config, pools []config.Pool, secrets secretSource, find bool,
	log *slog.Logger) (map[string]backend, error) {
	out := map[string]backend{}
	if cfg.Fake != nil {
		for _, p := range pools {
			fp := cfg.Fake.Pools[p.Name]
			out[p.Name] = backend{
				machines: providerfake.New(now, fp.Machines...),
				runners:  githubfake.New(now, fp.Runners...),
				demand:   func(context.Context) (*int, error) { return fp.Demand, nil },
			}
		}
		return out, nil
	}
	creds, err := secrets.read()
	if err != nil {
		return nil, op.Errorf(op.CodeInvalidConfig, "%v", err)
	}
	ghCreds := github.Credentials{AppPrivateKey: creds.AppPrivateKey, Token: creds.Token}
	if creds.AppPrivateKey != "" {
		if cfg.GitHub.App == nil {
			return nil, op.Errorf(op.CodeInvalidConfig, "a GitHub App key is given but github.app is not configured")
		}
		ghCreds.AppClientID, ghCreds.AppInstallationID = cfg.GitHub.App.ClientID, cfg.GitHub.App.InstallationID
	}
	client, err := github.NewClient(cfg.GitHub.URL, ghCreds, version, log)
	if err != nil {
		return nil, op.Errorf(op.CodeInvalidConfig, "%v", err)
	}
	cred, err := azureCredential()
	if err != nil {
		return nil, op.Errorf(op.CodeUnavailable, "azure: %v", err)
	}
	cloud := azure.Cloud{SubscriptionID: cfg.Azure.SubscriptionID, ResourceGroup: cfg.Azure.ResourceGroup,
		Location: cfg.Azure.Location, SubnetID: cfg.Azure.SubnetID}
	for _, p := range pools {
		set := client.ScaleSet(cfg.Instance, p.Name, p.RunnerGroup, p.Labels)
		if find {
			if err := set.Find(ctx); err != nil {
				return nil, op.Errorf(op.CodeUnavailable, "pool %s: %v", p.Name, err)
			}
		}
		shape := azure.Shape{Size: p.Azure.Size, Image: p.Azure.Image, Identity: p.Azure.Identity,
			EphemeralOSDisk: p.Azure.EphemeralOSDisk, OSDiskSizeGB: p.Azure.OSDiskSizeGB, Spot: p.Azure.Spot,
			AcceleratedNetworking: p.Azure.AcceleratedNetworking}
		if p.Azure.CustomData != "" {
			if shape.CustomData, err = os.ReadFile(p.Azure.CustomData); err != nil {
				return nil, op.Errorf(op.CodeInvalidConfig, "pool %s: customData: %v", p.Name, err)
			}
		}
		machines, err := azure.New(cloud, shape, cfg.Instance, p.Name, cred, nil)
		if err != nil {
			return nil, op.Errorf(op.CodeInvalidConfig, "pool %s: %v", p.Name, err)
		}
		maximum := p.Maximum
		out[p.Name] = backend{
			machines:    machines,
			runners:     set,
			demand:      set.Demand,
			listen:      func(ctx context.Context, sink github.Sink) { set.Listen(ctx, maximum, sink) },
			runnerGroup: set.Group(),
		}
	}
	return out, nil
}

// azureCredential signs in with workload identity, then a managed identity, then the Azure CLI; never with a client
// secret from the environment.
func azureCredential() (azcore.TokenCredential, error) {
	var chain []azcore.TokenCredential
	if wi, err := azidentity.NewWorkloadIdentityCredential(nil); err == nil {
		chain = append(chain, wi)
	}
	mi, err := azidentity.NewManagedIdentityCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("managed identity: %w", err)
	}
	cli, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure cli: %w", err)
	}
	chain = append(chain, mi, cli)
	return azidentity.NewChainedTokenCredential(chain, nil)
}
