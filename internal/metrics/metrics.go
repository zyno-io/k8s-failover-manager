package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	FailoverTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failover_transitions_total",
			Help: "Total number of failover transitions.",
		},
		[]string{"name", "namespace", "direction"},
	)

	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failover_reconcile_errors_total",
			Help: "Total number of reconcile errors by phase.",
		},
		[]string{"name", "namespace", "phase"},
	)

	RemoteSyncDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "failover_remote_sync_duration_seconds",
			Help:    "Duration of remote cluster sync operations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	ConnectionsKilledTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "failover_connection_kill_phases_total",
			Help: "Total number of connection kill phases completed (all agents ACKed successfully).",
		},
	)

	FailoverActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "failover_active",
			Help: "Whether failover is currently active (0 or 1).",
		},
		[]string{"name", "namespace"},
	)

	ActionExecutionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "failover_action_execution_duration_seconds",
			Help:    "Duration of individual action executions.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"name", "namespace", "action", "type"},
	)

	ActionExecutionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "failover_action_executions_total",
			Help: "Total number of action executions.",
		},
		[]string{"name", "namespace", "action", "type", "result"},
	)

	TransitionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "failover_transition_duration_seconds",
			Help:    "Duration of complete failover transitions (from start to finish).",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"name", "namespace", "direction"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		FailoverTransitionsTotal,
		ReconcileErrorsTotal,
		RemoteSyncDuration,
		ConnectionsKilledTotal,
		FailoverActive,
		ActionExecutionDuration,
		ActionExecutionsTotal,
		TransitionDuration,
	)
}
