package o11y

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"
	"os"
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
	msg := redact.Secrets(redact.InText(o11ylog.ErrorText(err), h.endpoints...), h.secrets...)
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

// otlpEnvCheck names one environment variable an OTLP exporter Init is
// about to build will read, and the rule that exporter's parser applies to
// it.
type otlpEnvCheck struct {
	name  string
	check func(string) (reason string, bad bool)
	// verbatim marks a variable the exporter reads as os.Getenv returns
	// it: the pinned log exporter never trims a value, so one wrapped in
	// whitespace is set, and is parsed with the whitespace. The trace and
	// metric exporters trim first and treat a blank value as unset.
	verbatim bool
	// pair, when set, is a second variable the exporter requires alongside
	// name before it reads either: a client certificate is loaded only
	// when both the CLIENT_CERTIFICATE and the CLIENT_KEY variable are set.
	pair string
	// fallback, when set, is the check the exporter applies instead when
	// name (and pair) is unset: the log exporter resolves a setting from
	// the signal variable first and consults the generic one only when the
	// signal variable is empty, so a generic value shadowed by a valid
	// signal value is never parsed and must not be checked.
	fallback *otlpEnvCheck
}

// set reports whether the exporter will read the variable: name is set and
// non-empty as the exporter sees it, and so is pair when the check has one.
func (c otlpEnvCheck) set() bool {
	return envValue(c.name, c.verbatim) != "" && (c.pair == "" || envValue(c.pair, c.verbatim) != "")
}

// envValue returns the variable name as the exporter that reads it will
// see it: verbatim, or trimmed of surrounding whitespace.
func envValue(name string, verbatim bool) string {
	v := os.Getenv(name)
	if verbatim {
		return v
	}
	return strings.TrimSpace(v)
}

// clientCertCheck builds the check for the CLIENT_CERTIFICATE / CLIENT_KEY
// pair under prefix, which the exporters read only when both are set.
func clientCertCheck(prefix string, verbatim bool) otlpEnvCheck {
	key := prefix + "CLIENT_KEY"
	return otlpEnvCheck{name: prefix + "CLIENT_CERTIFICATE", pair: key, verbatim: verbatim, check: checkClientKeyPair(key, verbatim)}
}

// otlpExporterEnvChecks lists the variables the enabled OTLP exporters will
// read from the environment, each with its exporter's own parsing rule. The
// two exporter families differ in when they read the environment. The
// pinned trace and metric exporters apply the environment before the
// explicit options, so with traces enabled, or metrics on the push path,
// every ENDPOINT, TIMEOUT, HEADERS, CERTIFICATE and CLIENT_CERTIFICATE /
// CLIENT_KEY variable under the generic and the signal prefix is read
// whatever Init passes in, and the metric exporter also reads its
// METRICS_TEMPORALITY_PREFERENCE and METRICS_DEFAULT_HISTOGRAM_AGGREGATION
// (their COMPRESSION variables map any other value to no compression
// without a message, so they are not checked). The pinned log exporter consults a variable only
// when the matching explicit option is absent: Init always passes the log
// endpoint URL, which also pins the path and the insecure flag, so its
// ENDPOINT and INSECURE variables are never read; it passes headers only
// when WithOTLPHeaders set some, so the HEADERS variables are read only
// then; and it never sets a timeout, a compression or a TLS configuration,
// so those variables are always read. It also resolves each setting from
// the signal variable first and reads the generic one only when the signal
// variable is unset (a signal value that fails to parse is echoed before
// it falls through, so it is still rejected); for the client certificate
// the signal pair counts as set only when both its variables are. The log
// exporter also reads every value verbatim, where the trace and metric
// exporters trim it, so its checks parse what os.Getenv returns. A
// variable no enabled exporter reads is left alone.
func otlpExporterEnvChecks(cfg *Config) []otlpEnvCheck {
	var checks []otlpEnvCheck
	envFirst := func(signal string) {
		for _, prefix := range []string{"OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_" + signal + "_"} {
			checks = append(checks,
				otlpEnvCheck{name: prefix + "ENDPOINT", check: checkURL},
				otlpEnvCheck{name: prefix + "TIMEOUT", check: checkMilliseconds},
				otlpEnvCheck{name: prefix + "HEADERS", check: malformedHeaderList},
				otlpEnvCheck{name: prefix + "CERTIFICATE", check: checkCertificateFile},
				clientCertCheck(prefix, false),
			)
		}
	}
	if cfg.traceEnabled {
		envFirst("TRACES")
	}
	if cfg.metricsEnabled && cfg.metricsOTLPEndpoint != "" {
		envFirst("METRICS")
		checks = append(checks,
			otlpEnvCheck{name: "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", check: checkTemporalityPreference},
			otlpEnvCheck{name: "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", check: checkHistogramAggregation},
		)
	}
	if cfg.logEnabled {
		const generic, logs = "OTEL_EXPORTER_OTLP_", "OTEL_EXPORTER_OTLP_LOGS_"
		signalFirst := func(setting string, check func(string) (string, bool)) otlpEnvCheck {
			return otlpEnvCheck{
				name:     logs + setting,
				check:    check,
				verbatim: true,
				fallback: &otlpEnvCheck{name: generic + setting, check: check, verbatim: true},
			}
		}
		clientCert := clientCertCheck(logs, true)
		genericClientCert := clientCertCheck(generic, true)
		clientCert.fallback = &genericClientCert
		checks = append(checks,
			signalFirst("TIMEOUT", checkMilliseconds),
			signalFirst("COMPRESSION", checkCompression),
			signalFirst("CERTIFICATE", checkCertificateFile),
			clientCert,
		)
		if len(cfg.otlpHeaders) == 0 {
			checks = append(checks, signalFirst("HEADERS", malformedHeaderList))
		}
	}
	return checks
}

// validateOTLPExporterEnv rejects a malformed OTLP exporter variable before
// any exporter is built. The pinned exporters parse their environment while
// they are constructed and report a value they cannot parse with the raw
// text attached: the trace and metric exporters through OTel's global
// logger ("input", "key" or "value"), the log exporter through otel.Handle
// with the value in the error. Init builds the exporters before it returns,
// so nothing the application can install later reaches those messages,
// and Logr's and ErrorHandler's redaction only covers what is reported
// after otel.SetLogger / otel.SetErrorHandler. Failing Init instead keeps
// the raw text out of stderr; the error names the variable and, for a
// header list, the pair's position, never the text. Only the variables the
// enabled exporters actually read are checked (see otlpExporterEnvChecks),
// with the rules their parsers apply: ENDPOINT must parse as a URL (a
// credential in its userinfo is what makes the echo dangerous), TIMEOUT
// must be an integer count of milliseconds, the log exporter's COMPRESSION
// must be "gzip" or "none", the metric exporter's TEMPORALITY_PREFERENCE
// and DEFAULT_HISTOGRAM_AGGREGATION must be one of the values it knows
// (it echoes any other through global.Warn, which a logger the
// application installed before Init would print), and each HEADERS pair
// must carry "=", a name that is an HTTP token and a value that is valid
// percent-encoding. CERTIFICATE must name
// a readable file holding a PEM certificate, and CLIENT_CERTIFICATE with
// CLIENT_KEY (read only when both are set) must name readable files that
// form a key pair: the exporters echo the path of a file they cannot read,
// both as a field and inside the os.ReadFile error. An empty variable is
// skipped, as the exporters skip it; each value is checked as the exporter
// that reads it will see it, trimmed or verbatim.
func validateOTLPExporterEnv(cfg *Config) error {
	for _, c := range otlpExporterEnvChecks(cfg) {
		for c.fallback != nil && !c.set() {
			c = *c.fallback
		}
		if !c.set() {
			continue
		}
		if err := validateOTLPEnvVar(c.name, envValue(c.name, c.verbatim), c.check); err != nil {
			return err
		}
	}
	return nil
}

func checkURL(v string) (string, bool) {
	if _, err := url.Parse(v); err != nil {
		return "it is not a valid URL", true
	}
	return "", false
}

func checkMilliseconds(v string) (string, bool) {
	if _, err := strconv.Atoi(v); err != nil {
		return "it is not an integer count of milliseconds", true
	}
	return "", false
}

func checkCompression(v string) (string, bool) {
	switch v {
	case "gzip", "none":
		return "", false
	}
	return `it is neither "gzip" nor "none"`, true
}

// checkTemporalityPreference applies the metric exporter's rule for
// METRICS_TEMPORALITY_PREFERENCE: one of its three selectors, compared
// case-insensitively as the exporter does.
func checkTemporalityPreference(v string) (string, bool) {
	switch strings.ToLower(v) {
	case "cumulative", "delta", "lowmemory":
		return "", false
	}
	return `it is none of "cumulative", "delta" and "lowmemory"`, true
}

// checkHistogramAggregation applies the metric exporter's rule for
// METRICS_DEFAULT_HISTOGRAM_AGGREGATION: one of its two histogram
// aggregations, compared case-insensitively as the exporter does.
func checkHistogramAggregation(v string) (string, bool) {
	switch strings.ToLower(v) {
	case "explicit_bucket_histogram", "base2_exponential_bucket_histogram":
		return "", false
	}
	return `it is neither "explicit_bucket_histogram" nor "base2_exponential_bucket_histogram"`, true
}

// checkCertificateFile applies the exporters' CA certificate rule: the
// path must be readable and hold at least one PEM certificate.
func checkCertificateFile(path string) (string, bool) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return "the file it names cannot be read", true
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return "the file it names holds no PEM certificate", true
	}
	return "", false
}

// checkClientKeyPair returns the exporters' client certificate rule for a
// CLIENT_CERTIFICATE value paired with the variable keyVar, read verbatim
// or trimmed as the exporter reads it: both files must be readable and
// together form a valid key pair.
func checkClientKeyPair(keyVar string, verbatim bool) func(string) (string, bool) {
	return func(certPath string) (string, bool) {
		cert, err := os.ReadFile(certPath)
		if err != nil {
			return "the file it names cannot be read", true
		}
		key, err := os.ReadFile(envValue(keyVar, verbatim))
		if err != nil {
			return "the file named by " + keyVar + " cannot be read", true
		}
		if _, err := tls.X509KeyPair(cert, key); err != nil {
			return "it and " + keyVar + " do not name a valid certificate and key pair", true
		}
		return "", false
	}
}

// validateConfiguredEndpoints rejects a malformed endpoint given through
// WithOTLPEndpoint or WithMetricsOTLPEndpoint before any exporter is built.
// The pinned trace and metric exporters' WithEndpointURL reports a URL it
// cannot parse through OTel's global logger with the raw text attached and
// then keeps their fallback endpoint, so Init would succeed while a
// credential in the URL's userinfo reached stderr. Only an endpoint an
// enabled OTLP exporter will use is checked, and the error names the
// option only: neither the value nor the parser's message, which quotes
// the offending part of the URL (a port, an escape, a host character),
// is repeated.
func validateConfiguredEndpoints(cfg *Config) error {
	type configured struct{ option, value string }
	var endpoints []configured
	if cfg.traceEnabled || cfg.logEnabled {
		endpoints = append(endpoints, configured{"WithOTLPEndpoint", cfg.otlpEndpoint})
	}
	if cfg.metricsEnabled && cfg.metricsOTLPEndpoint != "" {
		endpoints = append(endpoints, configured{"WithMetricsOTLPEndpoint", cfg.metricsOTLPEndpoint})
	}
	for _, e := range endpoints {
		if _, err := url.Parse(e.value); err != nil {
			return fmt.Errorf("o11y: the endpoint given to %s is not a valid URL; the OTLP exporters would report its raw text through OTel's global logger while Init builds them, so the option is rejected instead, and neither the value nor the parser's message, which quotes part of it, is repeated here", e.option)
		}
	}
	return nil
}

// validateConfiguredHeaders rejects a header name given through
// WithOTLPHeaders or WithProfilingAuthHeaders that is not an HTTP token.
// net/http refuses every request carrying such a name and quotes it,
// Go-escaped, in the error the exporter returns, which the ErrorHandler
// records; an escaped control character no longer matches the name in
// the redaction list, so the name is rejected up front and not repeated.
func validateConfiguredHeaders(cfg *Config) error {
	for _, h := range []struct {
		option  string
		headers map[string]string
	}{
		{"WithOTLPHeaders", cfg.otlpHeaders},
		{"WithProfilingAuthHeaders", cfg.profilingAuthHeaders},
	} {
		for name := range h.headers {
			if !isHTTPToken(name) {
				return fmt.Errorf("o11y: a header name given to %s is not a valid HTTP header name (an RFC 7230 token); net/http would reject every request carrying it and quote the name, escaped, in the export error, so the option is rejected instead and the name is not repeated here", h.option)
			}
		}
	}
	return nil
}

// validateOTLPEnvVar applies check to value, the variable name as the
// exporter reading it will see it, and returns the Init error for the
// reason check reports. An empty value is skipped.
func validateOTLPEnvVar(name, value string, check func(string) (reason string, bad bool)) error {
	if value == "" {
		return nil
	}
	if reason, bad := check(value); bad {
		return fmt.Errorf("o11y: %s cannot be used (%s); the OTLP exporters would report its raw text through OTel's global logger while Init builds them, before Logr() can be installed, so the variable is rejected instead", name, reason)
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

// goEscaped returns v as strconv.Quote renders it between the quotes, the
// form net/http and fmt's %q verb give a string in an error message.
func goEscaped(v string) string {
	q := strconv.Quote(v)
	return q[1 : len(q)-1]
}

// diagnosticSecrets lists the header names and values the diagnostics must
// never print: the names and values of WithOTLPHeaders and
// WithProfilingAuthHeaders, and
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
		// A rendering that quotes the value with %q escapes control and
		// non-printable characters, so that form is listed too whenever
		// it differs; a value the escaping leaves alone is added once.
		for _, s := range []string{v, goEscaped(v)} {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	// Names as well as values: a credential pasted in as a header name
	// reaches net/http, whose "invalid header field name" error echoes it
	// through the export error the ErrorHandler records.
	for k, v := range cfg.otlpHeaders {
		add(k)
		add(v)
	}
	for k, v := range cfg.profilingAuthHeaders {
		add(k)
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
