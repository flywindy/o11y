package o11y

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOTelErrorHandler_LogsOncePerWindow checks identical errors collapse to
// one ERROR per window while distinct errors are each logged.
func TestOTelErrorHandler_LogsOncePerWindow(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }

	refused := errors.New(`traces export: Post "http://otel-collector:4318/v1/traces": dial tcp 10.0.0.5:4318: connect: connection refused`)
	h.Handle(refused)
	h.Handle(refused)
	h.Handle(errors.New("logs export: something else"))
	now = now.Add(otelDiagnosticRepeatWindow)
	h.Handle(refused)

	out := buf.String()
	assert.Equal(t, 2, strings.Count(out, "connection refused"), "identical errors collapse to one line per window")
	assert.Equal(t, 1, strings.Count(out, "something else"))
	assert.Equal(t, 3, strings.Count(out, "level=ERROR"), "handled OTel errors must survive an error-only log level")
	assert.Contains(t, out, `msg="otel internal error"`)
	assert.Contains(t, out, "repeat_suppressed_for=1m0s")
}

// TestOTelErrorHandler_RedactsEndpointCredentials checks an endpoint quoted
// back in the error text loses its userinfo before it is logged.
func TestOTelErrorHandler_RedactsEndpointCredentials(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)), "http://svc:hunter2@collector:4318")

	h.Handle(errors.New(`traces export: Post "http://svc:hunter2@collector:4318/v1/traces": EOF`))

	assert.NotContains(t, buf.String(), "hunter2")
	assert.Contains(t, buf.String(), "collector:4318")
}

// TestOTelErrorHandler_NilIsIgnored checks nil errors and a nil logger are safe.
func TestOTelErrorHandler_NilIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	assert.NotPanics(t, func() { h.Handle(nil) })
	assert.Empty(t, buf.String())

	assert.NotPanics(t, func() { newOTelErrorHandler(nil).Handle(errors.New("x")) })
}

// TestShutdownSequence_DrainsTracesAndLogsBeforeMetrics pins the closer
// order: a batch that fails during the tracer's or logger's final flush is
// counted on the export-failure Recorder, and only a meter provider that
// shuts down afterwards still collects that count.
func TestShutdownSequence_DrainsTracesAndLogsBeforeMetrics(t *testing.T) {
	var order []string
	closer := func(name string) func(context.Context) error {
		return func(context.Context) error {
			order = append(order, name)
			return nil
		}
	}

	seq := shutdownSequence(closer("profiler"), closer("traces"), closer("logs"), closer("metrics-server"), closer("meter"))
	for _, fn := range seq {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"profiler", "traces", "logs", "metrics-server", "meter"}, order)

	order = nil
	for _, fn := range shutdownSequence(nil, closer("traces"), closer("logs"), closer("metrics-server"), closer("meter")) {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"traces", "logs", "metrics-server", "meter"}, order, "a nil profiler closer is skipped")
}
