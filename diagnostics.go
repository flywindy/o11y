package o11y

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

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
	// secrets are opaque values replaced wholesale, see diagnosticSecrets.
	secrets []string
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
	msg := redact.Secrets(redact.InText(err.Error(), h.endpoints...), h.secrets...)
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
// record behind it. The number of export calls that returned an error (a
// rejected or undeliverable batch, or a partial-success response) is
// available regardless of the handler as the o11y_export_failures_total
// metric.
func (s *SDK) ErrorHandler() otel.ErrorHandler {
	return s.errorHandler
}

// Logr returns a logr.Logger that writes OpenTelemetry's internal diagnostic
// messages — "dropped log records", an invalid instrument name — as
// structured records on the SDK's stdout log, one per distinct message per
// minute. Verbosity follows the OTel SDK's own convention: V(1) is WARN,
// V(4) is INFO, V(8) is DEBUG, gated by the SDK's log level.
//
// This surfaces more than the OTel default does, not just in a different
// format: the default logger is a stdr logger at verbosity 0, so it prints
// only error-level messages as plain text to stderr and drops the V(1)
// warnings and V(4) informational messages entirely. With Logr installed
// at the SDK's default INFO level those warnings and messages appear.
//
// The SDK does not install the logger (ADR 0003); the application does
// with otel.SetLogger(sdk.Logr()). See ErrorHandler for the companion.
func (s *SDK) Logr() logr.Logger {
	return s.logr
}

// newLogr builds the Logr diagnostics logger over logger; endpoints are
// redacted from error text as in newOTelErrorHandler.
func newLogr(logger *slog.Logger, endpoints, secrets []string) logr.Logger {
	return o11ylog.NewLogrRedacting(logger, repeat.NewSuppressor(otelDiagnosticRepeatWindow, maxTrackedOTelDiagnostics),
		o11ylog.Redaction{Endpoints: endpoints, Secrets: secrets})
}

// otlpHeaderEnvVars are the environment variables the pinned OTLP exporters
// read headers from. The SDK never sets them (ADR 0003); it reads them so
// their values can be redacted from diagnostics.
var otlpHeaderEnvVars = []string{
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
}

// otlpHeaderEnvVarsFor returns the header variables the OTLP exporters Init
// is about to build will read: the generic variable when any OTLP exporter
// is built, and the per-signal variable for each signal exported over OTLP.
// A variable no exporter reads is left alone.
func otlpHeaderEnvVarsFor(cfg *Config) []string {
	traces := cfg.traceEnabled
	logs := cfg.logEnabled
	metrics := cfg.metricsEnabled && cfg.metricsOTLPEndpoint != ""
	if !traces && !logs && !metrics {
		return nil
	}
	vars := []string{"OTEL_EXPORTER_OTLP_HEADERS"}
	if traces {
		vars = append(vars, "OTEL_EXPORTER_OTLP_TRACES_HEADERS")
	}
	if metrics {
		vars = append(vars, "OTEL_EXPORTER_OTLP_METRICS_HEADERS")
	}
	if logs {
		vars = append(vars, "OTEL_EXPORTER_OTLP_LOGS_HEADERS")
	}
	return vars
}

// validateOTLPHeaderEnv rejects a malformed OTLP header variable before any
// exporter is built. The pinned exporters parse these variables while they
// are constructed and report a pair they cannot parse through OTel's global
// logger with the raw text attached ("input", "key" or "value"), and Init
// builds the exporters before it returns, so nothing the application can
// install later reaches those messages: Logr's redaction only covers what
// is logged after otel.SetLogger. Failing Init instead keeps the raw text
// out of stderr; the error names the variable and the pair's position, not
// its text. The rules mirror the exporters' parser: a pair without "=", a
// name that is not an HTTP token, or a value that is not valid
// percent-encoding. An empty variable is skipped, as the exporters skip it.
func validateOTLPHeaderEnv(cfg *Config) error {
	for _, name := range otlpHeaderEnvVarsFor(cfg) {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		for i, pair := range strings.Split(raw, ",") {
			k, v, found := strings.Cut(pair, "=")
			var reason string
			switch {
			case !found:
				reason = "missing '='"
			case !isHTTPToken(strings.TrimSpace(k)):
				reason = "the name is not a valid HTTP header name"
			default:
				if _, err := url.PathUnescape(v); err != nil {
					reason = "the value is not valid percent-encoding"
				}
			}
			if reason != "" {
				return fmt.Errorf("o11y: %s: pair %d is malformed (%s); the OTLP exporters would report its raw text through OTel's global logger while Init builds them, before Logr() can be installed, so the variable is rejected instead", name, i+1, reason)
			}
		}
	}
	return nil
}

// isHTTPToken reports whether s is a non-empty RFC 7230 token, the check the
// pinned exporters apply to a header name from the environment.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c > unicode.MaxASCII {
			return false
		}
		if unicode.IsLetter(c) || unicode.IsDigit(c) || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

// diagnosticSecrets lists the header values the diagnostics must never
// print: the values of WithOTLPHeaders and WithProfilingAuthHeaders, and
// whatever the OTLP header environment variables hold. The pinned
// exporters parse OTEL_EXPORTER_OTLP_HEADERS themselves and, when a value
// fails to unescape, report it verbatim ("escape header value", "value",
// v) through the logger installed with otel.SetLogger; a bearer token has
// no "@" or endpoint for redact.InText to recognise, so it has to be named
// up front. Each variable contributes its whole value, each "k=v" pair, and
// the raw name and value parts with their unescaped forms, so the fragment
// the exporter echoes ("key" or "value") is covered whichever one it is. Every non-empty value is listed
// whatever its length: an exporter can echo a value on its own, so a short
// one is not safe to skip, and redact.Secrets replaces a short value only
// where it stands as a whole token so a "1" or "true" does not rewrite
// unrelated text.
func diagnosticSecrets(cfg *Config) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range cfg.otlpHeaders {
		add(v)
	}
	for _, v := range cfg.profilingAuthHeaders {
		add(v)
	}
	for _, name := range otlpHeaderEnvVars {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		add(raw)
		for pair := range strings.SplitSeq(raw, ",") {
			add(pair)
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			// The exporter reports a name that fails to unescape on its own
			// ("key", k), the same way it reports a value, and a credential
			// pasted into the wrong side of the "=" is still a credential.
			for _, part := range []string{k, v} {
				add(part)
				if unescaped, err := url.PathUnescape(strings.TrimSpace(part)); err == nil {
					add(unescaped)
				}
			}
		}
	}
	return out
}
