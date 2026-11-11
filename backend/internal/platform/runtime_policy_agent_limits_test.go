package platform

import "testing"

func TestAgentStepTimeoutEnvironmentBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"-1", 0}, {"0", 0}, {"1", 30}, {"29", 30}, {"30", 30}, {"90", 90}, {"3601", 3600},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CANVAS_AGENT_STEP_TIMEOUT_SECONDS", tc.value)
			if got := agentStepTimeoutSecondsDefault(); got != tc.want {
				t.Fatalf("timeout = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDefaultRuntimePolicyConcurrencyEnvironmentOverrides(t *testing.T) {
	t.Setenv("CANVAS_WORKER_CONCURRENCY", "")
	t.Setenv("CANVAS_CHANNEL_CONCURRENCY", "")
	t.Setenv("CANVAS_USER_TASK_CONCURRENCY", "")
	baseline := DefaultRuntimePolicy()
	if baseline.Task.WorkerConcurrency != 8 || baseline.Task.ChannelConcurrency != 8 || baseline.Task.ActiveTaskLimit != 5 {
		t.Fatalf("baseline concurrency = %+v, want worker=8 channel=8 active=5", baseline.Task)
	}
	if err := ValidateRuntimePolicy(baseline); err != nil {
		t.Fatalf("ValidateRuntimePolicy(baseline) error = %v", err)
	}

	t.Setenv("CANVAS_WORKER_CONCURRENCY", "16")
	t.Setenv("CANVAS_CHANNEL_CONCURRENCY", "10")
	t.Setenv("CANVAS_USER_TASK_CONCURRENCY", "4")
	overridden := DefaultRuntimePolicy()
	if overridden.Task.WorkerConcurrency != 16 || overridden.Task.ChannelConcurrency != 10 || overridden.Task.ActiveTaskLimit != 4 {
		t.Fatalf("overridden concurrency = %+v, want worker=16 channel=10 active=4", overridden.Task)
	}
	if err := ValidateRuntimePolicy(overridden); err != nil {
		t.Fatalf("ValidateRuntimePolicy(overridden) error = %v", err)
	}

	for _, tc := range []struct {
		name        string
		raw         string
		wantWorker  int
		wantChannel int
		wantActive  int
	}{
		{name: "zero_falls_back", raw: "0", wantWorker: 8, wantChannel: 8, wantActive: 5},
		{name: "negative_falls_back", raw: "-5", wantWorker: 8, wantChannel: 8, wantActive: 5},
		{name: "invalid_falls_back", raw: "invalid", wantWorker: 8, wantChannel: 8, wantActive: 5},
		{name: "minimum_valid", raw: "1", wantWorker: 1, wantChannel: 1, wantActive: 1},
		{name: "maximum_valid", raw: "999", wantWorker: 999, wantChannel: 999, wantActive: 999},
		{name: "above_maximum_clamped", raw: "5000", wantWorker: 999, wantChannel: 999, wantActive: 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CANVAS_WORKER_CONCURRENCY", tc.raw)
			t.Setenv("CANVAS_CHANNEL_CONCURRENCY", tc.raw)
			t.Setenv("CANVAS_USER_TASK_CONCURRENCY", tc.raw)
			policy := DefaultRuntimePolicy()
			if policy.Task.WorkerConcurrency != tc.wantWorker || policy.Task.ChannelConcurrency != tc.wantChannel || policy.Task.ActiveTaskLimit != tc.wantActive {
				t.Fatalf("raw=%q -> concurrency = (worker=%d channel=%d active=%d), want (worker=%d channel=%d active=%d)",
					tc.raw,
					policy.Task.WorkerConcurrency, policy.Task.ChannelConcurrency, policy.Task.ActiveTaskLimit,
					tc.wantWorker, tc.wantChannel, tc.wantActive,
				)
			}
			if err := ValidateRuntimePolicy(policy); err != nil {
				t.Fatalf("ValidateRuntimePolicy(raw=%q) error = %v", tc.raw, err)
			}
		})
	}
}
