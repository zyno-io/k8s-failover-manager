package v1alpha1

import "testing"

func TestPhaseOrder(t *testing.T) {
	tests := []struct {
		name  string
		phase FailoverPhase
		want  int
	}{
		{name: "idle", phase: PhaseIdle, want: 0},
		{name: "pre-actions", phase: PhaseExecutingPreActions, want: 1},
		{name: "update", phase: PhaseUpdatingResources, want: 2},
		{name: "flush", phase: PhaseFlushingConnections, want: 3},
		{name: "post-actions", phase: PhaseExecutingPostActions, want: 4},
		{name: "unknown", phase: FailoverPhase("nope"), want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PhaseOrder(tt.phase); got != tt.want {
				t.Fatalf("PhaseOrder(%q) = %d, want %d", tt.phase, got, tt.want)
			}
		})
	}
}
