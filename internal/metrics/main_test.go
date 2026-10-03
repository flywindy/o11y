package metrics

import (
	"os"
	"testing"
)

// TestMain clears OTEL_METRICS_EXEMPLAR_FILTER, which the meter providers
// InitMeter builds read: the exemplar tests need the default trace-based
// filter, and a shell or CI that sets always_off would otherwise fail them.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("OTEL_METRICS_EXEMPLAR_FILTER")
	os.Exit(m.Run())
}
