package o11y

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"

	o11ylog "github.com/flywindy/o11y/internal/log"
	"github.com/flywindy/o11y/internal/redact"
	"github.com/flywindy/o11y/internal/repeat"
)

// otelDiagnosticRepeatWindow is how long an identical OTel-internal error or
// message is suppressed after it was last logged. A collector outage makes
// the BatchSpanProcessor report the same connection error every few seconds
// and the log BatchProcessor every second; one line per window says the
// same thing without drowning the log the outage is being debugged from.
const otelDiagnosticRepeatWindow = time.Minute

// maxTrackedOTelDiagnostics bounds the suppression tables. Distinct messages
// are rare (one per failing endpoint, one per kind of dropped data), so the
// bound is a memory guard, not a working-set size.
const maxTrackedOTelDiagnostics = 32

// otelErrorHandler turns the errors OTel components hand to otel.Handle into
// structured ERROR records, one per distinct error per repeat window. ERROR,
// not WARN: a service running WithLogLevel(slog.LevelError) would otherwise
// filter out the very export failures the handler exists to surface.
type otelErrorHandler struct {
	logger    *slog.Logger
	suppress  *repeat.Suppressor
	now       func() time.Time
	endpoints []string
}

// newOTelErrorHandler returns a handler writing to logger. endpoints are the
// configured export endpoints, passed to redact.InText so an error that
// quotes one back (net/url does, verbatim) cannot leak embedded credentials.
func newOTelErrorHandler(logger *slog.Logger, endpoints ...string) *otelErrorHandler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &otelErrorHandler{
		logger:    logger,
		suppress:  repeat.NewSuppressor(otelDiagnosticRepeatWindow, maxTrackedOTelDiagnostics),
		now:       time.Now,
		endpoints: endpoints,
	}
}

// Handle implements otel.ErrorHandler.
func (h *otelErrorHandler) Handle(err error) {
	if err == nil {
		return
	}
	msg := redact.InText(err.Error(), h.endpoints...)
	if h.suppress.SuppressedAt(msg, h.now()) {
		return
	}
	h.logger.ErrorContext(context.Background(), "otel internal error",
		slog.String("error", msg),
		slog.String("repeat_suppressed_for", otelDiagnosticRepeatWindow.String()),
	)
}

// ErrorHandler returns a handler that writes the errors OpenTelemetry
// components report internally — a failed OTLP export, an instrument the
// SDK could not register — as structured ERROR records on the SDK's stdout
// log, one per distinct error per minute. Without it those errors go to
// the OTel default handler, which prints plain text to stderr on every
// occurrence: during a collector outage that is one unparseable line every
// few seconds per pod, in the log the outage is being debugged from.
//
// The SDK does not install the handler (ADR 0003: no global state); the
// application does, next to the other globals it wires:
//
//	otel.SetErrorHandler(sdk.ErrorHandler())
//	otel.SetLogger(sdk.Logr())
//
// Records go to stdout only, not through the OTLP log pipeline: an export
// failure logged through the pipeline that is failing would queue another
// record behind it. The number of failed batches is available regardless
// of the handler as the o11y_export_failures_total metric.
func (s *SDK) ErrorHandler() otel.ErrorHandler {
	return s.errorHandler
}

// Logr returns a logr.Logger that writes OpenTelemetry's internal diagnostic
// messages — "dropped log records", an invalid instrument name — as
// structured records on the SDK's stdout log, one per distinct message per
// minute. Verbosity follows the OTel SDK's own convention: V(1) is WARN,
// V(4) is INFO, V(8) is DEBUG, gated by the SDK's log level. Without it the
// OTel default prints plain text to stderr.
//
// The SDK does not install the logger (ADR 0003); the application does
// with otel.SetLogger(sdk.Logr()). See ErrorHandler for the companion.
func (s *SDK) Logr() logr.Logger {
	return s.logr
}

// newLogr builds the Logr diagnostics logger over logger; endpoints are
// redacted from error text as in newOTelErrorHandler.
func newLogr(logger *slog.Logger, endpoints ...string) logr.Logger {
	return o11ylog.NewLogr(logger, repeat.NewSuppressor(otelDiagnosticRepeatWindow, maxTrackedOTelDiagnostics), endpoints...)
}
