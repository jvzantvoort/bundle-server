[![forthebadge](https://forthebadge.com/images/badges/made-with-crayons.svg)](https://forthebadge.com)
[![forthebadge](https://forthebadge.com/images/badges/designed-in-etch-a-sketch.svg)](https://forthebadge.com)
[![forthebadge](https://forthebadge.com/images/badges/you-didnt-ask-for-this.svg)](https://forthebadge.com)

**CAUTION**: this is a project with which I'm investigating using AI
as a tool for coding. All of this code is generated.

# Bundle Server

Bundle Server provides an HTTP API and Swagger UI for Bundle content-addressed
file collections.

## Run

```bash
cp config.yaml.example config.yaml
go run ./cmd/bundle-server serve --config config.yaml
```

The Swagger UI is available at the configured `server.swagger_path` (default
`/swagger/`). The OpenAPI document is served at `server.openapi_path` (default
`/openapi.yaml`).

## API specification

`openapi.yaml` here is the canonical API description. The bundle CLI
([bundle](https://github.com/jvzantvoort/bundle)) and the web UI
([bundle-ui](https://github.com/jvzantvoort/bundle-ui)) each keep a copy, and
their OpenAPI workflow fails when it differs from this file on `main`. After an
API change is merged, run `scripts/sync-openapi.sh` in both repositories.

## Development

```bash
go test -race ./...
go vet ./...
go build ./cmd/bundle-server
```

CI runs these on Linux, macOS and Windows with the current Go, and on Linux
with the oldest Go that `go.mod` allows. It also checks `gofmt -s`, that
`go.mod` and `go.sum` are tidy, and the GoReleaser configuration, and runs
`govulncheck`. It runs weekly as well, to catch new vulnerabilities. A release
tag (`vX.Y.Z`) is only published after CI passes, and is built with the
current Go.

`bundle-server config validate --config config.yaml` validates the server
configuration before startup.

## Tags

Tags are 1-64 letters, digits, `.`, `_` or `-`, stored in lowercase. Bundle list
and query results include each bundle's tags. Besides adding and removing tags
on a bundle (`POST`/`DELETE …/bundles/{checksum}/tags`), the API can:

| Request | Effect |
| --- | --- |
| `PUT …/bundles/{checksum}/tags/{tag}` with `{"tag": "new"}` | Rename a tag on one bundle in a single write (needs `If-Match`) |
| `GET …/pools/{pool}/tags` | List the tags in a pool with the number of bundles for each |
| `PUT …/pools/{pool}/tags/{tag}` with `{"tag": "new"}` | Rename a tag on every bundle in the pool |
| `DELETE …/pools/{pool}/tags/{tag}` | Remove a tag from every bundle in the pool |

The pool-wide requests report how many bundles changed and which failed; repeating
one finishes what a failure left undone.

## Media metadata

`bundle update --media` (bundle v0.5.0 and later) and
[bundle-python](https://github.com/jvzantvoort/bundle-python) `scan` store the
EXIF data, resolution, capture time, GPS position and device of a bundle's images
and videos in `.bundle/MEDIA.json`. That file is outside the bundle checksum, and
the server only reads it, with the bundle library's `media` package:

| Request | Returns |
| --- | --- |
| `GET …/bundles/{checksum}/media` | Devices with file counts, and every media file without its EXIF data |
| `GET …/bundles/{checksum}/media/{filePath}` | One file, including all EXIF tags |

Both return `404` with code `media_not_found` when there is no media metadata. A
bundle's `links.media` is only present when it has `MEDIA.json`.

## systemd deployment

Build the production binary, install its runtime files, and create the
unprivileged service account:

```bash
go build -trimpath -ldflags="-s -w" -o bundle-server ./cmd/bundle-server
sudo useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin bundle-server
sudo install -Dm755 bundle-server /usr/local/bin/bundle-server
sudo install -Dm644 openapi.yaml /usr/share/bundle-server/openapi.yaml
sudo install -Dm640 config.yaml /etc/bundle-server/config.yaml
sudo install -Dm644 deploy/systemd/bundle-server.service /etc/systemd/system/bundle-server.service
sudo install -d -o bundle-server -g bundle-server /srv/bundles
```

Set pool roots and any bearer token in `/etc/bundle-server/config.yaml`. The
included service grants write access only to `/srv/bundles`; add each different
configured pool root to `ReadWritePaths` in
`/etc/systemd/system/bundle-server.service`.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now bundle-server
sudo systemctl status bundle-server
```

## Resumable uploads

Upload sessions are stored in the pool root next to their staging directory
(`.upload-<id>/`, `.upload-<id>.json` and a `.upload-<id>.done` journal of the
files received), so they survive a restart. Repeating `POST …/uploads` with the
same `Idempotency-Key` and request body returns the existing session, whose
`missing_files` lists what still needs to be sent; the same key with a different
body returns `409`. At startup, expired sessions and partially written files are
removed. `bundle push` uses this to continue an interrupted push.

## Scheduled actions

Configure recurring work in `schedules` using Go duration intervals. Supported
actions are `verify` for integrity checks, `collect_status` for pool status
aggregation, and `cache_metadata` for refreshing the in-memory bundle metadata
cache. Each action is executed through the server task queue.

The queue runs `server.task_workers` tasks at a time. Further tasks, from
schedules or from verify requests, wait until a worker is free; the request
itself returns at once with the queued task. At most 1000 tasks can wait: beyond
that, verify returns `503` and a schedule logs that it skipped a run. Tasks
still waiting at shutdown are marked failed.
