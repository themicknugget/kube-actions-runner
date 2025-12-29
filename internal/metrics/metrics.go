package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	namespace = "webhook_scaler"
)

var (
	WebhookRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_requests_total",
			Help:      "Total number of webhook requests received",
		},
		[]string{"status", "owner", "repo"},
	)

	WebhookLatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "webhook_latency_seconds",
			Help:      "Latency of webhook request processing in seconds",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"owner", "repo"},
	)

	RunnerJobsCreatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "runner_jobs_created_total",
			Help:      "Total number of runner jobs created",
		},
		[]string{"owner", "repo", "mode"},
	)

	RunnerJobsActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "runner_jobs_active",
			Help:      "Number of currently active runner jobs",
		},
	)

	GitHubAPIRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "github_api_requests_total",
			Help:      "Total number of GitHub API requests",
		},
		[]string{"endpoint", "status"},
	)

	GitHubAPILatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "github_api_latency_seconds",
			Help:      "Latency of GitHub API requests in seconds",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"endpoint"},
	)

	GitHubAPIRateLimitRemaining = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "github_api_rate_limit_remaining",
			Help:      "Remaining GitHub API rate limit",
		},
	)
)

func StatusCategory(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "unknown"
	}
}
