// Package metrics registers and exposes Prometheus metrics for env-forge.
// Call Register() once at startup; the metrics are then available on the
// default Prometheus registry and served via promhttp.Handler().
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// StepDuration tracks how long each saga step takes to execute or compensate.
	StepDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "envforge",
			Subsystem: "step",
			Name:      "duration_seconds",
			Help:      "Saga step execution duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"step", "operation", "result"}, // operation: execute|compensate, result: ok|error
	)

	// SagaOutcome counts completed sagas by terminal status.
	SagaOutcome = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "envforge",
			Subsystem: "saga",
			Name:      "outcomes_total",
			Help:      "Total number of sagas by terminal outcome.",
		},
		[]string{"outcome"}, // outcome: success|failed|aborted
	)

	// HTTPRequestDuration tracks HTTP handler latency by method, route, and status code.
	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "envforge",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"method", "route", "status"},
	)
)

// Handler returns an http.Handler that serves the Prometheus metrics page.
func Handler() http.Handler {
	return promhttp.Handler()
}

// statusRecorder wraps http.ResponseWriter to capture the written status code.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// InstrumentHandler wraps h to record HTTPRequestDuration for the given route label.
func InstrumentHandler(route string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		timer := prometheus.NewTimer(prometheus.ObserverFunc(func(d float64) {
			HTTPRequestDuration.WithLabelValues(r.Method, route, http.StatusText(rec.code)).Observe(d)
		}))
		defer timer.ObserveDuration()
		h.ServeHTTP(rec, r)
	})
}
