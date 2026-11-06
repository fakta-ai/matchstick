# matchstick

Ephemeral GitHub Actions runners: one fresh cloud VM per job.

matchstick watches GitHub for jobs that want your self-hosted runners, creates a new virtual machine for each job,
lets the job run on it, and deletes the machine afterwards. No runner ever sees a second job, so nothing carries over:
no files, caches, credentials or processes from earlier jobs. It keeps no database; the cloud and GitHub are its whole
state, so it can be restarted at any time.

It creates the runners as Azure virtual machines and registers them with GitHub through runner scale sets.

> [!WARNING]
> Under construction: not usable yet. No release has been published.

## What it supports

- Several pools in one process, each with its own labels, VM size, image, identity (or none), maximum number of
  machines, idle spares and maximum lifetime.
- Azure VMs from a Compute Gallery image, with a private address only, an optional ephemeral OS disk on the VM's
  local storage, and Spot priority that falls back to regular priority when Spot has no capacity.
- No secret in the VMs' settings: each VM claims its runner registration from matchstick, proving which VM it is with
  Azure's signed attested document, and gets it once.
- GitHub organisations, repositories and enterprises, on github.com or GitHub Enterprise Server, with a GitHub App or
  a token.
- Prometheus metrics, a health check, and a command line with JSON in and out (`plan`, `status`, `drain`), also served
  over JSON-RPC and MCP by `matchstick-adapter`.

## Quick start

1. Make, beforehand: a subnet for the runners, a resource group for the VMs, an image that meets the
   [image contract](docs/image-contract.md) in a Compute Gallery, a GitHub App installed on your organisation with
   **Self-hosted runners: read and write**, and an Azure identity for matchstick with the roles in
   [docs/configuration.md](docs/configuration.md#azure).
2. Write a configuration, starting from [examples/azure.yaml](examples/azure.yaml). `claimURL` is where the VMs reach
   matchstick's claim endpoint (`listen.claim`, port 8443 by default), so the runners' subnet must reach it.
3. Put the App's private key in the secret mount, `/var/run/secrets/matchstick/github-app-private-key`.
4. See what it would do, then run it:

   ```sh
   matchstick plan --config matchstick.yaml
   matchstick run --config matchstick.yaml
   ```

5. In a workflow, ask for a pool's label: `runs-on: linux-x64-large`.

On Kubernetes, the chart in [deploy/chart/matchstick](deploy/chart/matchstick) runs one replica signed in to Azure
with workload identity.

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

- [Configuration](docs/configuration.md)
- [Operations](docs/operations.md): running, watching, draining
- [The image contract](docs/image-contract.md)
- [Architecture](docs/architecture.md), for contributors

## License

[MIT](LICENSE).
