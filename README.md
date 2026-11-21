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

## Development

```bash
go test ./...
go vet ./...
go build ./cmd/bundle-server
```

`bundle-server config validate --config config.yaml` validates the server
configuration before startup.

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
