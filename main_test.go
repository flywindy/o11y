package o11y

import (
	"os"
	"testing"
)

// shellDependentEnv lists the sampler and pillar-toggle variables Init reads:
// the OTel SDK's sampler decides whether test spans are recorded (and an
// invalid value makes Init fail), and the O11Y_*_ENABLED toggles switch whole
// pillars off. Other OTEL_* variables Init validates or the SDK reads (batch
// delays, span limits, exemplar filter) are not cleared here. TestMain clears them so a developer's shell or a CI job
// cannot switch a pillar off or silence spans underneath the tests; a test
// that exercises one sets it with t.Setenv.
var shellDependentEnv = []string{
	"OTEL_TRACES_SAMPLER",
	"OTEL_TRACES_SAMPLER_ARG",
	"O11Y_TRACE_ENABLED",
	"O11Y_METRICS_ENABLED",
	"O11Y_LOG_ENABLED",
	"O11Y_PROFILING_ENABLED",
}

func TestMain(m *testing.M) {
	for _, key := range shellDependentEnv {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
