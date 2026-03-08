package metrics

import (
	"testing"

	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestMetricsAreRegistered(t *testing.T) {
	// Instantiate vectors/counters so they are surfaced by Gather().
	FailoverTransitionsTotal.WithLabelValues("test", "default", "to-failover").Inc()
	ReconcileErrorsTotal.WithLabelValues("test", "default", "service").Inc()
	RemoteSyncDuration.Observe(0.001)
	ConnectionsKilledTotal.Inc()
	FailoverActive.WithLabelValues("test", "default").Set(1)
	ActionExecutionDuration.WithLabelValues("test", "default", "action", "http").Observe(0.001)
	ActionExecutionsTotal.WithLabelValues("test", "default", "action", "http", "success").Inc()
	TransitionDuration.WithLabelValues("test", "default", "failover").Observe(0.001)

	gathered, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	seen := make(map[string]bool, len(gathered))
	for _, mf := range gathered {
		seen[mf.GetName()] = true
	}

	expected := []string{
		"failover_transitions_total",
		"failover_reconcile_errors_total",
		"failover_remote_sync_duration_seconds",
		"failover_connection_kill_phases_total",
		"failover_active",
		"failover_action_execution_duration_seconds",
		"failover_action_executions_total",
		"failover_transition_duration_seconds",
	}

	for _, name := range expected {
		if !seen[name] {
			t.Fatalf("expected metric %q to be registered", name)
		}
	}
}
