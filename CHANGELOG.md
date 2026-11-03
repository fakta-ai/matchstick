# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- The architecture, in `docs/architecture.md`.
- The planner: given a pool's machines, runner registrations and demand, decides which machines to delete (finished,
  expired, never registered, surplus), which registrations to remove (orphans) and how many machines to create,
  within the pool's maximum.
- One pool's loop: gathers the pool's machines and registrations, decides, and acts: removes registrations before
  deleting their machines, creates a registration and then its machine, removes the registration of a machine the
  cloud refused, pauses creation after capacity errors and after machines that never register.
- The `decide`, `plan` and `status` commands, with JSON Schemas for input and output, `--json`, `--input`, and
  `--help --json` listing them as MCP tools.
- A fake cloud and GitHub, configured in the configuration file: `matchstick plan --config examples/fake.yaml`.
- Prometheus metrics for each pool.
- `matchstick run`: serves every configured pool with GitHub runner scale sets and Azure virtual machines; metrics and
  health on `listen.metrics`, drain on `listen.admin`.
- `matchstick drain`: drains a pool of the running process, or stops draining it.
- The Azure provider: private network interface, Compute Gallery image, identity per pool or none, ephemeral OS disk,
  Spot with regular fallback, quota and allocation failures reported as retry-later.
- The GitHub credential is read from standard input or the secret mount.
- `docs/configuration.md`, `docs/operations.md`, `examples/azure.yaml`.
