# Operations

## Running

```sh
matchstick run --config matchstick.yaml
```

Run one copy. Replace it rather than run two at once (on Kubernetes: one replica, `Recreate`). A restart is safe at any
time: the next start finds every machine and registration again.

## Watching

- `/healthz` on `listen.metrics`: 200 while every pool has completed a round within three intervals.
- `/metrics`: `matchstick_demand`, `matchstick_machines{state}`, `matchstick_runners{status}`,
  `matchstick_creations_total`, `matchstick_creation_failures_total{kind}`, `matchstick_deletions_total{reason}`,
  `matchstick_registration_removals_total{reason}`, `matchstick_rounds_total{result}`,
  `matchstick_round_duration_seconds`, `matchstick_last_success_timestamp_seconds`, `matchstick_creation_paused`,
  `matchstick_draining`, each with a `pool` label.
- Logs: JSON lines on stdout, one per action, with `pool`, `machine` and `reason`.

Suggested alerts: `time() - matchstick_last_success_timestamp_seconds > 300`; `matchstick_creation_paused == 1` for
more than 30 minutes; `increase(matchstick_deletions_total{reason="unregistered"}[1h]) > 3`.

## Looking

```sh
matchstick status --config matchstick.yaml --json   # machines, registrations, demand, per pool
matchstick plan --config matchstick.yaml --json     # what a round would delete and create now
```

Both read the cloud and GitHub with the same configuration and secret mount as `run`, and change nothing.

## Draining a pool

```sh
matchstick drain --config matchstick.yaml --pool build          # stop taking jobs, give up idle runners
matchstick drain --config matchstick.yaml --pool build --stop   # back to normal
```

`drain` talks to the running process on `listen.admin`, a loopback address, so it runs where `run` runs. On Kubernetes:

```sh
kubectl exec deploy/matchstick -- matchstick drain --config /etc/matchstick/matchstick.yaml --pool build
```

`status` shows the pool empty once its busy runners have finished. A drain lasts until the process restarts; to keep a
pool empty, set its `maximum` to 0.

## Cost

A machine that powered itself off still bills until matchstick deletes it, which it does on the next round. While
matchstick is stopped, finished machines keep billing, within each pool's maximum, until it starts again.
