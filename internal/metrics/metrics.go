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

	reg.MustRegister(reqCount, reqDuration, inFlight)

	return &Registry{
		Registry:        reg,
		RequestCount:    reqCount,
		RequestDuration: reqDuration,
		InFlight:        inFlight,
	}
}

func HandlerFor(reg *Registry) http.Handler {
	return promhttp.HandlerFor(reg.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
