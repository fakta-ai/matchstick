# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- The architecture, in `docs/architecture.md`.
- The planner: given a pool's machines, runner registrations and demand, decides which machines to delete (finished,
  expired, never registered, surplus), which registrations to remove (orphans) and how many machines to create,
  within the pool's maximum.
