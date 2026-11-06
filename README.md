# matchstick

Ephemeral GitHub Actions runners: one fresh cloud VM per job.

matchstick watches GitHub for jobs that want your self-hosted runners, creates a new virtual machine for each job,
lets the job run on it, and deletes the machine afterwards. No runner ever sees a second job, so nothing carries over:
no files, caches, credentials or processes from earlier jobs. It keeps no database; the cloud and GitHub are its whole
state, so it can be restarted at any time.

It creates the runners as Azure virtual machines and registers them with GitHub through runner scale sets.

> [!NOTE]
> Early releases (0.x): a minor version may change the configuration. [CHANGELOG.md](CHANGELOG.md) says what each
> release changed; releases are on the [releases page](https://github.com/fakta-ai/matchstick/releases).

## What it supports

- Several pools in one process, each with its own labels, VM size, image, identity (or none), maximum number of
  machines, idle spares and maximum lifetime.
- Azure Linux VMs from a Compute Gallery image, with a private address only, an optional ephemeral OS disk on the
  VM's local storage, and Spot priority that falls back to regular priority when Spot has no capacity.
- No secret in the VMs' settings: each VM claims its runner registration from matchstick, proving which VM it is with
  Azure's signed attested document, and gets it once.
- GitHub organisations and repositories with a GitHub App or a token, and enterprises with a token, on github.com,
  GitHub Enterprise Cloud or GitHub Enterprise Server.
- Creating the GitHub App with exactly the permissions it needs (`matchstick github-app create`), and checking it
  (`matchstick github-app check`).
- Prometheus metrics, a health check, and a command line with JSON in and out (`plan`, `status`, `drain`,
  `github-app create`, `github-app check`), also served over JSON-RPC and MCP by `matchstick-adapter`.

## Getting started

[docs/setup.md](docs/setup.md) takes you from nothing to a first job: create and install the GitHub App, store its
key, prepare Azure (resource group, subnet, runner image, role, workload identity), install the signed Helm chart,
check the setup, run a workflow. In short:

```sh
matchstick github-app create --org acme --name acme-runners --key-file secrets/github-app-private-key
# install the App with the installURL it prints; store the key in a Kubernetes Secret; prepare Azure
matchstick github-app check --config matchstick.yaml --secrets-dir secrets
helm install matchstick oci://ghcr.io/fakta-ai/charts/matchstick --version <version> \
  --namespace matchstick -f values.yaml
```

Then, in a workflow, ask for a pool's label: `runs-on: acme-linux`.

Outside Kubernetes, `matchstick run --config matchstick.yaml` runs it with the key in
`/var/run/secrets/matchstick/github-app-private-key`; `matchstick plan` first shows what it would do.

Try the planner without a cloud or GitHub:

```sh
matchstick plan --config examples/fake.yaml
```

## Building

```sh
nix build                 # matchstick and matchstick-adapter in ./result/bin
go build ./cmd/...        # with Go 1.27.3 or later
```

## Documentation

- [Setting up](docs/setup.md): from nothing to a first job
- [Configuration](docs/configuration.md)
- [Operations](docs/operations.md): running, watching, draining
- [The image contract](docs/image-contract.md)
- [Threat model](docs/threat-model.md): what matchstick protects, whom it trusts, what is left to you
- [Architecture](docs/architecture.md), for contributors

## License

[MIT](LICENSE).
