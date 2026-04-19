# stravaKudos

Posts kudos to new activities of everyone you follow and everyone who
follows you. Runs in the background on a Linux server with a 1-request-
per-90-seconds drip so it never trips Strava's rate limiter.

Uses Strava's mobile API (`m.strava.com`) with the Android client's
hardcoded `client_id=2` + your own account password. The public v3 API
does not expose the kudos or feed endpoints required.

## Repo layout

```
cmd/stravakudos/         # main binary
internal/
  config/                # env loader
  strava/                # HTTP client, endpoints, auth manager
  store/                 # SQLite persistence (modernc.org/sqlite, pure Go)
  scheduler/             # thin-drip scheduler (feed tick + roster + GC)
  ratelimit/             # global pause gate (1h on 429)
deploy/                  # systemd unit, env template, migrate.sh, rollback.sh
docs/
  operations.md          # day-to-day runbook
  plans/                 # design plans
  problems-and-solutions.md  # v1 incident postmortem (context)
  architecture.md        # diagrams of v1 (legacy)
  strava-api.md          # API notes
_legacy/                 # v1 source and binaries, kept for reference
```

## Build

```sh
make test              # unit + e2e
make build             # local dev binary in bin/
make build-linux       # static linux/amd64 binary for rw
make deploy-package    # dist/stravakudos-deploy.tar.gz (bin + systemd + scripts)
```

Requires Go 1.23+.

## Run locally

Populate `.env` in the repo root (same keys as
`deploy/stravakudos.env.example`) then:

```sh
STATE_DIR=/tmp/stravakudos-dev make build && ./bin/stravakudos
```

## Deploy to the server

See `deploy/README.md` for the full migration flow. Summary:

```sh
make deploy-package
scp dist/stravakudos-deploy.tar.gz rw:/root/
ssh rw 'tar -xzf /root/stravakudos-deploy.tar.gz -C /tmp/sk-deploy'
ssh rw 'vim /etc/stravakudos/stravakudos.env'   # fill in secrets
ssh rw 'bash /tmp/sk-deploy/migrate.sh'
```

Operations: `docs/operations.md` (logs, SQL queries, token rotation,
upgrades, 429 handling).

## Strava API credentials

Get `CLIENT_SECRET` from https://www.strava.com/settings/api. This is
the *mobile* password-grant shared secret; `client_id=2` is hardcoded
in the binary (extracted from the official Android client).

