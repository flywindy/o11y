package o11y

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"strconv"
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
	msg := redact.Secrets(redact.InText(errorText(err), h.endpoints...), h.secrets...)
	if h.suppress.SuppressedAt(msg, h.now()) {
		return
	}
	h.logger.ErrorContext(context.Background(), "otel internal error",
		slog.String("error", msg),
		slog.String("repeat_suppressed_for", otelDiagnosticRepeatWindow.String()),
	)
}

// errorText renders err for the diagnostic record without letting a broken
// error value take the process down: a typed nil pointer is named rather
// than dereferenced by its own Error method, and an Error method that
// panics is recovered into a placeholder naming the type. A diagnostic is
// never worth a crash.
func errorText(err error) (text string) {
	if rv := reflect.ValueOf(err); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return fmt.Sprintf("<nil %T>", err)
	}
	defer func() {
		if r := recover(); r != nil {
			text = fmt.Sprintf("[omitted: %T panicked while rendering]", err)
		}
	}()
	return err.Error()
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

// otlpExporterEnvPrefixes returns the OTEL_EXPORTER_OTLP_ variable
// prefixes the OTLP exporters Init is about to build will read: the generic
// prefix when any OTLP exporter is built, and the per-signal prefix for
// each signal exported over OTLP. A variable no exporter reads is left
// alone.
func otlpExporterEnvPrefixes(cfg *Config) []string {
	traces := cfg.traceEnabled
	logs := cfg.logEnabled
	metrics := cfg.metricsEnabled && cfg.metricsOTLPEndpoint != ""
	if !traces && !logs && !metrics {
		return nil
	}
	prefixes := []string{"OTEL_EXPORTER_OTLP_"}
	if traces {
		prefixes = append(prefixes, "OTEL_EXPORTER_OTLP_TRACES_")
	}
	if metrics {
		prefixes = append(prefixes, "OTEL_EXPORTER_OTLP_METRICS_")
	}
	if logs {
		prefixes = append(prefixes, "OTEL_EXPORTER_OTLP_LOGS_")
	}
	return prefixes
}

// validateOTLPExporterEnv rejects a malformed OTLP exporter variable before
// any exporter is built. The pinned exporters parse their environment while
// they are constructed, before the explicit options apply, and report a
// value they cannot parse through OTel's global logger with the raw text
// attached ("input", "key" or "value"); Init builds the exporters before it
// returns, so nothing the application can install later reaches those
// messages, and Logr's redaction only covers what is logged after
// otel.SetLogger. Failing Init instead keeps the raw text out of stderr;
// the error names the variable and, for a header list, the pair's position,
// never the text. The rules mirror the exporters' parsers: ENDPOINT must
// parse as a URL (a credential in its userinfo is what makes the echo
// dangerous), TIMEOUT must be an integer count of milliseconds, and each
// HEADERS pair must carry "=", a name that is an HTTP token and a value that
// is valid percent-encoding. Certificate variables name files, which the
// exporters echo as paths, and INSECURE and COMPRESSION are never echoed,
// so they are not checked. An empty variable is skipped, as the exporters
// skip it.
func validateOTLPExporterEnv(cfg *Config) error {
	for _, prefix := range otlpExporterEnvPrefixes(cfg) {
		if err := validateOTLPEnvVar(prefix+"ENDPOINT", func(v string) (string, bool) {
			if _, err := url.Parse(v); err != nil {
				return "it is not a valid URL", true
			}
			return "", false
		}); err != nil {
			return err
		}
		if err := validateOTLPEnvVar(prefix+"TIMEOUT", func(v string) (string, bool) {
			if _, err := strconv.Atoi(v); err != nil {
				return "it is not an integer count of milliseconds", true
			}
			return "", false
		}); err != nil {
			return err
		}
		if err := validateOTLPEnvVar(prefix+"HEADERS", malformedHeaderList); err != nil {
			return err
		}
	}
	return nil
}

// validateOTLPEnvVar applies check to the trimmed value of the variable
// name when it is set and non-empty, and returns the Init error for the
// reason check reports.
func validateOTLPEnvVar(name string, check func(string) (reason string, bad bool)) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	if reason, bad := check(raw); bad {
		return fmt.Errorf("o11y: %s is malformed (%s); the OTLP exporters would report its raw text through OTel's global logger while Init builds them, before Logr() can be installed, so the variable is rejected instead", name, reason)
	}
	return nil
}

// malformedHeaderList mirrors the exporters' header-list parser on a
// HEADERS value and names the first malformed pair by position.
func malformedHeaderList(raw string) (string, bool) {
	for i, pair := range strings.Split(raw, ",") {
		k, v, found := strings.Cut(pair, "=")
		switch {
		case !found:
			return fmt.Sprintf("pair %d is missing '='", i+1), true
		case !isHTTPToken(strings.TrimSpace(k)):
			return fmt.Sprintf("the name of pair %d is not a valid HTTP header name", i+1), true
		}
		if _, err := url.PathUnescape(v); err != nil {
			return fmt.Sprintf("the value of pair %d is not valid percent-encoding", i+1), true
		}
	}
	return "", false
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
