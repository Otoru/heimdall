# Heimdall – Agent Quickstart

This repo is a Maven-compatible HTTP server backed by S3. Key capabilities:

- S3 storage with optional prefix/path-style; computes SHA1/MD5 on upload and background repair.
- Auth: Optional Basic Auth **or** `X-API-Key` header (OR logic — either method passes). API key validated via external HTTP endpoint (`AUTH_API_KEY_ENDPOINT`); token sent as `Authorization` header (`AUTH_API_KEY_TOKEN`). Auth disabled when all three env vars are unset.
- Prometheus metrics on a dedicated listener.
- Maven proxy with S3 cache: on-demand fetch from upstream (e.g., Maven Central), catalog browsing via parsed HTML listings, and no chained checksum generation when fetching checksum files.
- Proxy management API: `GET/POST /proxies` (create), `PUT/DELETE /proxies/{name}` (update/delete). Proxy configs live in S3 under `__proxycfg__/`.
- Group endpoint: `/packages/{path}` serves Maven artifacts by checking local first, then proxies (Maven-compatible). Catalog `path=packages/...` merges local + proxy listings.
- Catalog: `GET /catalog?path=...&limit=...` returns entries (`file`/`dir`/`proxy`), including proxy paths.
- Swagger UI at `/swagger/`; docs generated with `swag` (`cmd/heimdall/main.go`).

Packaging and releases:

- App image published to GHCR on tag `vX.Y.Z` (multi-arch): `ghcr.io/otoru/heimdall:<version>` and `:latest`.
- Helm chart published to GHCR OCI on tag `chart-X.Y.Z`: `oci://ghcr.io/otoru/heimdall-chart/heimdall` (defaults to appVersion image).
- Current chart version: see `charts/heimdall/Chart.yaml` (autoscaling disabled by default).

Config (envs):

- `S3_BUCKET` (required), `S3_REGION` (default `us-east-1`), `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_USE_PATH_STYLE`, `S3_PREFIX`.
- `SERVER_ADDR` (default `:8080`), `METRICS_ADDR` (default `:9090`), `AUTH_USERNAME`, `AUTH_PASSWORD`.
- `AUTH_API_KEY_ENDPOINT` — base URL for API key validation (`GET <endpoint>/licenses/valid?id=<key>`).
- `AUTH_API_KEY_TOKEN` — token sent in the `Authorization` header when calling the API key endpoint. Inject via `envSecrets` in Helm, not plain `env`.
- `CHECKSUM_SCAN_INTERVAL`, `CHECKSUM_SCAN_PREFIX`.

Testing:

- `CGO_ENABLED=0 go test ./...` (macOS Xcode license can otherwise block CGO).

Notes for changes:

- Update swagger after handler annotations: `swag init -g cmd/heimdall/main.go -o cmd/internal/docs`. Security definitions: `BasicAuth` (basic) and `ApiKeyAuth` (apiKey, header `X-API-Key`).
- Avoid committing `internal/docs/swagger.json|yaml` (ignored). Only `docs.go` is tracked.
- Chart HPA is off by default; set `autoscaling.enabled=true` to enable.
- Checklist per change:
  - Run `CGO_ENABLED=0 go test ./...` (or equivalent) before commit/release.
  - Update docs/swagger via `swag init -g cmd/heimdall/main.go -o internal/docs` after changing handlers/annotations.
  - Reflect behavior/API changes in `README.md` and `agents.md`.
  - Bump versions with semver: app tags `vX.Y.Z` (image), chart tags `chart-X.Y.Z` (chart `appVersion` matches image tag).
  - Versioning guidance: patch = fixes/non-breaking; minor = new compatible features; major = breaking changes; chart semver follows chart impact and appVersion tracks the image.
