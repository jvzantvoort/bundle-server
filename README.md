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
