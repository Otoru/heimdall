# Heimdall

Lightweight Maven-compatible HTTP server in Go. Serves artifacts from an S3-compatible bucket (AWS/OCI/MinIO), supports proxying upstream Maven repos with S3 cache, logs in JSON (zap), and exposes Prometheus metrics.

## Features (quick view)

| Area | Details |
| --- | --- |
| Storage | S3-compatible, optional prefix, path-style toggle |
| Auth | Optional Basic Auth for all routes except `/healthz` |
| Metrics | `/metrics` on a dedicated listener |
| Logging | JSON via zap |
| Checksums | Auto-generate SHA1/MD5 on upload and background repair |
| Proxy | Upstream Maven proxy with S3 cache; browse via catalog |

## Configuration (env vars)

| Variable | Default | Required | Description |
| --- | --- | --- | --- |
| `S3_BUCKET` | — | yes | Target bucket name. |
| `S3_REGION` | `us-east-1` | no | Bucket region. |
| `S3_ENDPOINT` | — | no | Custom S3 endpoint (OCI/MinIO, etc.). |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | — | no | Explicit credentials; SDK default chain if empty. |
| `S3_USE_PATH_STYLE` | `false` | no | `true` to force path-style requests. |
| `S3_PREFIX` | — | no | Prefix inside the bucket for all objects. |
| `SERVER_ADDR` | `:8080` | no | Main HTTP listener (artifacts). |
| `METRICS_ADDR` | `:9090` | no | Metrics listener (`/metrics`). |
| `AUTH_USERNAME` | — | no | Enables Basic Auth when paired with password. |
| `AUTH_PASSWORD` | — | no | Password for Basic Auth. |
| `CHECKSUM_SCAN_INTERVAL` | — | no | Background checksum repair interval (e.g. `10m`); empty disables. |
| `CHECKSUM_SCAN_PREFIX` | — | no | Limit checksum repair scan to a prefix. |

## Endpoints

| Path | Method | Purpose |
| --- | --- | --- |
| `/healthz` | GET | Liveness probe. |
| `/metrics` | GET | Prometheus metrics (on `METRICS_ADDR`). |
| `/catalog` | GET | Lists entries (non-recursive) with `type` = `file`/`dir`/`proxy`. |
| `/proxies` | GET/POST | List or add proxy repositories. |
| `/proxies/{name}` | PUT/DELETE | Update or delete a proxy. |
| `/packages/{any}` | GET/HEAD | Group view: search local, then proxies (Maven-compatible). |
| `/{any}` | GET/HEAD/PUT | Maven artifact fetch/head/upload mapped to S3 key. |

## Run locally

```bash
export S3_BUCKET=my-bucket
export S3_REGION=us-east-1
# export S3_ENDPOINT=https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
# export S3_USE_PATH_STYLE=true
# export AUTH_USERNAME=ci-user
# export AUTH_PASSWORD=change-me

go run ./cmd/heimdall
```

Upload:

```bash
curl -T my-artifact.jar \
  http://localhost:8080/releases/com/acme/app/1.0.0/app-1.0.0.jar
```

Download/check:

```bash
curl -I http://localhost:8080/releases/com/acme/app/1.0.0/app-1.0.0.jar
```

### Proxy repositories (cache in S3)

Add a proxy (persisted as `__proxycfg__/<name>.json` in S3, shared by all replicas):

```bash
curl -u user:pass -X POST http://localhost:8080/proxies \
  -H 'Content-Type: application/json' \
  -d '{"name":"central","url":"https://repo.maven.apache.org/maven2"}'
```

Browse: `curl -u user:pass http://localhost:8080/catalog` shows proxies with `type: "proxy"`.
Listing a proxy path (`path=central/...`) shows upstream directory entries (non-recursive) even before caching.

Fetch via proxy (cached to S3 on first hit):

```bash
curl -I http://localhost:8080/central/org/apache/maven/maven/3.9.6/maven-3.9.6.pom
```

## Docker

```bash
docker build -t heimdall .
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e S3_BUCKET=my-bucket \
  -e S3_REGION=us-east-1 \
  -e S3_ENDPOINT=https://<namespace>.compat.objectstorage.<region>.oraclecloud.com \
  -e S3_USE_PATH_STYLE=true \
  heimdall
```

Build arg `GO_VERSION` can override the Go toolchain (default 1.25).

## Docker Compose

```bash
cp .env.example .env  # adjust values
docker compose up --build
```

## CI/CD

- App releases (tags `vX.Y.Z`) build multi-arch images to GHCR: `ghcr.io/otoru/heimdall:<version>` and `:latest`.
- Chart releases (tags `chart-X.Y.Z`) package and push to GHCR OCI: `oci://ghcr.io/otoru/heimdall-chart/heimdall`.
- PRs run `go test ./...`.

## Maven config snippet

```xml
<repository>
  <id>heimdall</id>
  <url>http://localhost:8080/releases</url>
</repository>
```

For Basic Auth, add a `<server>` entry in `settings.xml` with `id` matching the repository.

## Notes

- Object keys mirror the request path (optional `S3_PREFIX` prepended).
- Response headers propagate `Content-Type`, `ETag`, `Last-Modified`, and `Content-Length` when available.
- For OCI/other S3-compat, set `S3_ENDPOINT` and typically `S3_USE_PATH_STYLE=true`.
- Metrics include request counters, duration histograms, and inflight gauges. Logs are JSON.

## Helm chart

Charts live under `charts/heimdall`. Package version is tracked in `Chart.yaml` and overridden at release time with the GitHub tag (semver).

| Key | Description |
| --- | --- |
| `image.repository` / `image.tag` | Container image (defaults to `ghcr.io/otoru/heimdall:<appVersion>`; set `image.tag` to override). |
| `service.port` / `service.metricsPort` | HTTP and metrics ports. |
| `autoscaling.*` | HPA settings (CPU/memory utilization targets, min/max replicas). |
| `ingress.*` | Ingress host/paths/class/tls. |
| `env` / `envSecrets` / `envConfigMaps` | Direct env vars, secret-backed env vars, and ConfigMap-backed env vars. |

Basic install:

```bash
helm install heimdall charts/heimdall \
  --set env.S3_BUCKET=my-bucket \
  --set env.S3_REGION=us-east-1
```

From GHCR (OCI):

```bash
helm registry login ghcr.io -u <user> -p <token>
helm install heimdall oci://ghcr.io/otoru/heimdall-chart/heimdall \
  --version 0.5.6 \
  --set env.S3_BUCKET=my-bucket \
  --set env.S3_REGION=us-east-1
```

If you want to pull manually:

```bash
helm pull oci://ghcr.io/otoru/heimdall-chart/heimdall --version 0.5.6
```

Notes:

- Metrics are exposed on port `9090` with Prometheus annotations on the pod; scrape via service `metrics` port.
- HPA is disabled by default; set `autoscaling.enabled=true` to turn it on.
