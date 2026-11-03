# Contributing

Thank you for helping.

## Before you start

- For anything larger than a fix, open an issue first and describe the problem, so we can agree on the approach
  before you write code.
- Read [docs/architecture.md](docs/architecture.md). A change that adds remembered state, a dependency or an
  extension point is discussed in an issue first.
- Security problems: see [SECURITY.md](SECURITY.md), not an issue.

## Setting up

With [Nix](https://nixos.org/), `nix develop` gives a shell with Go, golangci-lint, govulncheck and Helm, and
`nix build` builds both executables and runs the tests. Without Nix, you need Go (the version in `go.mod`) and
[golangci-lint](https://golangci-lint.run/) v2.

```sh
go vet ./...
go test -race ./...
golangci-lint run ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
helm lint deploy/chart/matchstick
nix build .#matchstick
```

CI runs these on every pull request, in the flake's dev shell. When `go.sum` changes, update `vendorHash` in
`nix/package.nix` (`nix build` prints the right one). Nothing needs a cloud account or a GitHub App: the tests use
in-memory fakes, and `matchstick plan --config examples/fake.yaml` runs the planner against a fake pool.

## How the code is organised

- The decisions are pure functions in `internal/plan`: they take what was observed and return what to do. Put new
  rules there, with table tests.
- Everything that talks to the outside (cloud, GitHub, files, HTTP, the terminal) is a thin layer around them.
- Interfaces exist only where there are two implementations: `provider.Provider` (Azure and the in-memory fake) and
  `github.Runners` (GitHub and its fake). Supporting another cloud means implementing `provider.Provider`: create,
  list and delete one pool's machines. Please don't add options or hooks nobody uses yet.
- Every operation is declared once with JSON Schemas for its input and output (`internal/op`); the command line and
  the `rpc`/`mcp` adapters are built from those declarations. A new operation or field starts there. Output fields are
  only ever added, never renamed or removed.
- Secrets are read only from standard input or the platform's secret mount, and never logged or printed.

## Tests

Test behaviour: given these machines and runners, the pool deletes this and creates that; after a restart halfway, it
converges. Use the fakes in `internal/provider/fake` and `internal/github/fake`. A bug fix comes with a test that
fails without it.

## Pull requests

- Titles and commits follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
- Update `docs/` and the `Unreleased` section of `CHANGELOG.md` when behaviour changes.
- Keep each pull request to one concern. Pull requests are merged with a merge commit once CI passes and review
  threads are resolved.
- Dependabot opens a weekly pull request moving `flake.lock`'s inputs, each to a commit at least 14 days old. One that
  changes only `flake.lock` needs no review and merges by itself once CI passes.

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
