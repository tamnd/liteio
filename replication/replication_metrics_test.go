// SPDX-License-Identifier: Apache-2.0

package replication

import "testing"

// TestReplicationMetricsCount verifies that IncReplicated and IncReplFailed
// increment the counters returned by ReplicationStats.
func TestReplicationMetricsCount(t *testing.T) {
	// Reset counters for this test (process-wide, so start from current value).
	before, beforeFailed := ReplicationStats()

	IncReplicated()
	IncReplicated()
	IncReplFailed()

	got, gotFailed := ReplicationStats()
	if got != before+2 {
		t.Errorf("replicated = %d, want %d", got, before+2)
	}
	if gotFailed != beforeFailed+1 {
		t.Errorf("failed = %d, want %d", gotFailed, beforeFailed+1)
	}
}
