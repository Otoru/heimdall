# Heimdall

Lightweight Maven-compatible HTTP server written in Go. It proxies `GET`, `HEAD`, and `PUT` directly to an S3-compatible bucket (AWS or OCI), emits JSON logs with `zap`, and exposes Prometheus metrics on a dedicated listener.

## Features

- S3-compatible storage (AWS/OCI/MinIO) with optional path-style and prefix.
- Basic Auth gate (optional) for all routes except `/healthz`.
- Structured JSON logging via `zap` (production encoder).
- Prometheus metrics (`/metrics`) plus standard Go/process collectors.

## Configuration

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

Endpoints:

| Path | Method | Purpose |
| --- | --- | --- |
| `/healthz` | GET | Liveness probe. |
| `/metrics` | GET | Prometheus metrics (on `METRICS_ADDR`). |
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

GitHub Actions builds and pushes a multi-arch image to Docker Hub whenever an **app release** (tags like `v1.2.3`) is published. Required repository secrets:

| Secret | Description |
| --- | --- |
| `DOCKERHUB_USERNAME` | Docker Hub username. |
| `DOCKERHUB_TOKEN` | Docker Hub access token with push rights. |

Images are tagged with the release tag (semver) and `latest`. The image name is `otoru/heimdall`.

Helm charts are released independently: publish a release with tag format `chart-X.Y.Z` and the chart is packaged and pushed to GHCR (`ghcr.io/otoru/heimdall-chart`) and attached to the GitHub release.

Pull requests run `go test ./...` automatically via GitHub Actions to block broken code before merging.

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
helm install heimdall oci://ghcr.io/otoru/heimdall-chart \
  --version 1.0.0 \
  --set env.S3_BUCKET=my-bucket \
  --set env.S3_REGION=us-east-1
```

Notes:

- Metrics are exposed on port `9090` with Prometheus annotations on the pod; scrape via service `metrics` port.
- HPA is enabled by default; adjust `autoscaling` values to fit your cluster.
