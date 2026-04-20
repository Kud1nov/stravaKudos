# stravaKudos

Posts kudos to new activities of followers and followees on Strava.
Runs in the background on a Linux server with conservative request
pacing to stay well under Strava's rate limits.

Uses Strava's internal mobile API with the Android client's
`client_id=2`. The public v3 API does not expose the kudos / feed /
followers endpoints required.

> ⚠️ Personal-use automation. Running automated kudos may violate
> Strava's Terms of Service; run only on your own account and at your
> own risk.

## Repo layout

```
cmd/stravakudos/      # main binary
internal/
  config/             # env loader
  strava/             # HTTP client, endpoints, auth manager
  store/              # SQLite persistence (modernc.org/sqlite, pure Go)
  scheduler/          # thin-drip scheduler (feed tick + roster + GC)
  ratelimit/          # global pause gate (1h on 429)
_legacy/              # v1 source, kept for reference
```

Operational docs, deploy scripts, and plans are kept off the public
tree.

## Build

```sh
make test          # unit + e2e
make build         # local dev binary → bin/stravakudos
make build-linux   # static linux/amd64 binary → bin/stravakudos-linux-amd64
```

Requires Go 1.23+.

## Run locally

Create a `.env` in the repo root with:

```sh
USER_EMAIL=<your strava email>
USER_PASSWORD=<your strava password>
CLIENT_SECRET=<from https://www.strava.com/settings/api>
STATE_DIR=/tmp/stravakudos-dev
LOG_LEVEL=debug
```

Then:

```sh
make build && ./bin/stravakudos
```

`.env` and `*.db` are gitignored — never commit them.
