package voice

import (
	"testing"
	"time"
)

func TestReconnectsOutlastAShortNetworkOutage(t *testing.T) {
	var total time.Duration
	for a := 1; a <= maxReconnects; a++ {
		d := reconnectDelay(a)
		if d > maxBackoff {
			t.Fatalf("attempt %d waits %s, more than the %s cap", a, d, maxBackoff)
		}
		total += d
	}
	if total < 90*time.Second {
		t.Fatalf("all reconnects span %s; a 30-40 s outage must not exhaust them", total)
	}
	if reconnectDelay(1) != time.Second || reconnectDelay(5) != maxBackoff {
		t.Fatalf("backoff should start at 1 s and reach the cap by attempt 5, got %s and %s", reconnectDelay(1), reconnectDelay(5))
	}
}
