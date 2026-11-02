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

## Scheduled actions

Configure recurring work in `schedules` using Go duration intervals. Supported
actions are `verify` for integrity checks, `collect_status` for pool status
aggregation, and `cache_metadata` for refreshing the in-memory bundle metadata
cache. Each action is executed through the server task queue.
