# matchstick

Ephemeral GitHub Actions runners: one fresh cloud VM per job.

matchstick watches GitHub for jobs that want your self-hosted runners, creates a new virtual machine for each job,
lets the job run on it, and deletes the machine afterwards. No runner ever sees a second job, so nothing carries over:
no files, caches, credentials or processes from earlier jobs. It keeps no database; the cloud and GitHub are its whole
state, so it can be restarted at any time.

It is built on GitHub's runner scale-set protocol, with a cloud-neutral core. Azure is the first cloud.

> [!WARNING]
> Under construction: not usable yet. [docs/architecture.md](docs/architecture.md) describes what is being built.

## License

[MIT](LICENSE).
