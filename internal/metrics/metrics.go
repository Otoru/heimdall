package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Registry struct {
	Registry        *prometheus.Registry
	RequestCount    *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	InFlight        prometheus.Gauge

	// ProxyConfigCache counts how proxy definition lookups were satisfied:
	// hit, refresh, stale or error. A rising "stale" rate means the object
	// store is flaky; "error" means requests are being failed outright.
	ProxyConfigCache *prometheus.CounterVec
	// ProxyFetch counts upstream artifact fetches by proxy and outcome:
	// fetched, notfound, mismatch or error.
	ProxyFetch *prometheus.CounterVec
	// UpstreamRetries counts retried upstream requests. Cross-referencing it
	// with build failures is how a degraded upstream gets spotted early.
	UpstreamRetries prometheus.Counter
}

func New() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)

	reqCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "heimdall_http_requests_total",
			Help: "Total de requisições HTTP por método e status.",
		},
		[]string{"code", "method"},
	)

	reqDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "heimdall_http_request_duration_seconds",
			Help:    "Duração das requisições HTTP.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"code", "method"},
	)

	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "heimdall_http_inflight_requests",
		Help: "Quantidade de requisições em andamento.",
	})

	proxyConfigCache := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "heimdall_proxy_config_cache_total",
			Help: "Proxy definition lookups by outcome (hit, refresh, stale, error).",
		},
		[]string{"result"},
	)

	proxyFetch := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "heimdall_proxy_fetch_total",
			Help: "Upstream artifact fetches by proxy and outcome (fetched, notfound, mismatch, error).",
		},
		[]string{"proxy", "result"},
	)

	upstreamRetries := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "heimdall_upstream_retries_total",
		Help: "Upstream requests that were retried after a transport error, 429 or 5xx.",
	})

	reg.MustRegister(reqCount, reqDuration, inFlight, proxyConfigCache, proxyFetch, upstreamRetries)

	return &Registry{
		Registry:         reg,
		RequestCount:     reqCount,
		RequestDuration:  reqDuration,
		InFlight:         inFlight,
		ProxyConfigCache: proxyConfigCache,
		ProxyFetch:       proxyFetch,
		UpstreamRetries:  upstreamRetries,
	}
}

func HandlerFor(reg *Registry) http.Handler {
	return promhttp.HandlerFor(reg.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
