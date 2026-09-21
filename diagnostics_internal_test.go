package o11y

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y/internal/redact"
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

// TestOTelErrorHandler_RedactsSecrets checks a configured header value is
// replaced in the error text even though nothing about it looks like an
// endpoint.
func TestOTelErrorHandler_RedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	h.secrets = []string{"BearerSecret%zz"}

	h.Handle(errors.New(`escape header value: invalid URL escape "%zz" in BearerSecret%zz`))

	assert.NotContains(t, buf.String(), "BearerSecret")
	assert.Contains(t, buf.String(), "[redacted]")

	buf.Reset()
	h.secrets = diagnosticSecrets(&Config{otlpHeaders: map[string]string{"x-tiny": "a%zz"}})
	h.Handle(errors.New(`escape header value: invalid URL escape "%zz" in a%zz (attempt 12)`))

	assert.NotContains(t, buf.String(), "a%zz", "a short configured value echoed on its own is redacted")
	assert.Contains(t, buf.String(), "attempt 12", "text around it is left alone")
}

// TestDiagnosticSecrets collects the configured header names and values and
// the environment-provided ones: whole variable, each pair, each name and
// value with their unescaped forms, deduplicated, short values included,
// empty ones left out.
func TestDiagnosticSecrets(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890, BearerSecret%zz=oops")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	cfg := &Config{
		otlpHeaders:          map[string]string{"x-api-key": "configured-secret", "x-tiny": "ab"},
		profilingAuthHeaders: map[string]string{"authorization": "Basic cHJvZmlsZXM="},
	}

	got := diagnosticSecrets(cfg)

	for _, want := range []string{
		"configured-secret", "Basic cHJvZmlsZXM=",
		"authorization=Bearer%20abcdef, x-short=1, api-key=k-1234567890, BearerSecret%zz=oops",
		"authorization=Bearer%20abcdef", "Bearer%20abcdef", "Bearer abcdef",
		"api-key=k-1234567890", "k-1234567890",
		"x-short=1", "1", "ab",
		"BearerSecret%zz=oops", "BearerSecret%zz", "authorization", "api-key",
		"x-api-key", "x-tiny",
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "", "an empty value is not a secret")
	assert.Len(t, got, len(slices.Compact(slices.Sorted(slices.Values(got)))), "no duplicates")
}

// TestValidateOTLPExporterEnv mirrors the pinned exporters' parsers and
// their reading order: with traces or push-path metrics enabled every
// ENDPOINT, TIMEOUT and HEADERS variable under the generic and the signal
// prefix is checked; the log exporter reads a variable only where Init
// passes no explicit option, so its endpoint is never checked, its headers
// only without WithOTLPHeaders, and its timeout and compression always. A
// malformed value fails with an error that names the variable (and the
// pair's position) but never its text; an empty variable is ignored.
func TestValidateOTLPExporterEnv(t *testing.T) {
	all := &Config{traceEnabled: true, logEnabled: true, metricsEnabled: true, metricsOTLPEndpoint: "http://collector:4318"}

	t.Run("well-formed", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20abcdef, x-tenant=acme")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "10000")
		t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "Delta")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", "base2_exponential_bucket_histogram")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "brotli")
		t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "5000")
		t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "64")
		t.Setenv("OTEL_TRACES_SAMPLER", "ParentBased_TraceIDRatio")
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0.25")
		t.Setenv("OTEL_BLRP_EXPORT_TIMEOUT", "30000")
		t.Setenv("OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT", "128")
		t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "60000")
		require.NoError(t, validateOTLPExporterEnv(all), "the trace exporter maps an unknown compression to none without a message")
	})

	for _, tc := range []struct{ name, variable, value, reason, secret string }{
		{"missing equals", "OTEL_EXPORTER_OTLP_HEADERS", "x-ok=1, authorization", "pair 2 is missing '='", "authorization"},
		{"bad name", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "x-ok=1, bad name=value", "name of pair 2 is not a valid HTTP header name", "bad name"},
		{"bad value", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-ok=1, authorization=BearerSecret%zz", "value of pair 2 is not valid percent-encoding", "BearerSecret"},
		{"bad endpoint", "OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:secret%zz@collector:4318", "not a valid URL", "secret%zz"},
		{"bad metrics endpoint", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://user:secret%zz@collector:4318", "not a valid URL", "secret%zz"},
		{"bad timeout", "OTEL_EXPORTER_OTLP_TIMEOUT", "10s", "not an integer count of milliseconds", "10s"},
		{"bad logs compression", "OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "brotli", `neither "gzip" nor "none"`, "brotli"},
		{"bad metrics temporality", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "sometimes", `none of "cumulative", "delta" and "lowmemory"`, "sometimes"},
		{"bad histogram aggregation", "OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION", "sketch", `neither "explicit_bucket_histogram" nor "base2_exponential_bucket_histogram"`, "sketch"},
		{"bad span batcher delay", "OTEL_BSP_SCHEDULE_DELAY", "soon", "not an integer", "soon"},
		{"bad span limit", "OTEL_SPAN_EVENT_COUNT_LIMIT", "many", "not an integer", "many"},
		{"bad generic attribute limit", "OTEL_ATTRIBUTE_COUNT_LIMIT", "lots", "not an integer", "lots"},
		{"bad sampler", "OTEL_TRACES_SAMPLER", "BearerSecret", "not one of the sampler names", "BearerSecret"},
		{"bad log batcher timeout", "OTEL_BLRP_EXPORT_TIMEOUT", "BearerSecret", "not an integer", "BearerSecret"},
		{"bad log record limit", "OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT", "long", "not an integer", "long"},
		{"bad metric interval", "OTEL_METRIC_EXPORT_INTERVAL", "never", "not a positive integer", "never"},
		{"non-positive metric timeout", "OTEL_METRIC_EXPORT_TIMEOUT", "-5", "not a positive integer", "-5"},
		{"bad cardinality limit", "OTEL_GO_X_CARDINALITY_LIMIT", "BearerSecret", "not an integer", "BearerSecret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.variable, tc.value)
			err := validateOTLPExporterEnv(all)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.variable+" cannot be used")
			assert.Contains(t, err.Error(), tc.reason)
			assert.NotContains(t, err.Error(), tc.secret, "the raw text stays out of the error")
		})
	}

	t.Run("variable no exporter reads is ignored", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "authorization=BearerSecret%zz")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://user:secret%zz@collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "sometimes")
		pull := &Config{traceEnabled: true, logEnabled: true, metricsEnabled: true}
		require.NoError(t, validateOTLPExporterEnv(pull), "the metrics variables are read only on the OTLP push path")
		require.Error(t, validateOTLPExporterEnv(all))
		none := &Config{}
		require.NoError(t, validateOTLPExporterEnv(none), "no OTLP exporter, nothing to validate")
	})

	t.Run("log exporter reads only what Init leaves to the environment", func(t *testing.T) {
		logsOnly := &Config{logEnabled: true}
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:secret%zz@collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://user:secret%zz@collector:4318")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_INSECURE", "maybe")
		require.NoError(t, validateOTLPExporterEnv(logsOnly), "Init always passes the log endpoint URL, which pins endpoint, path and insecure")

		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=BearerSecret%zz")
		require.Error(t, validateOTLPExporterEnv(logsOnly), "without WithOTLPHeaders the log exporter reads the headers variable")
		withHeaders := &Config{logEnabled: true, otlpHeaders: map[string]string{"x-api-key": "configured"}}
		require.NoError(t, validateOTLPExporterEnv(withHeaders), "explicit headers take precedence and the variable is never parsed")

		t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "soon")
		require.Error(t, validateOTLPExporterEnv(withHeaders), "the log exporter always reads the timeout")

		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "10000")
		require.NoError(t, validateOTLPExporterEnv(withHeaders), "a valid signal value shadows the generic one, which the exporter never reads")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "later")
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "10000")
		err := validateOTLPExporterEnv(withHeaders)
		require.Error(t, err, "a signal value that fails to parse is echoed before the exporter falls through")
		assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT cannot be used")
	})

	t.Run("certificate files", func(t *testing.T) {
		dir := t.TempDir()
		certPath, keyPath := writeTestKeyPair(t, dir)
		missing := filepath.Join(dir, "missing.pem")
		text := filepath.Join(dir, "notes.txt")
		require.NoError(t, os.WriteFile(text, []byte("not a certificate"), 0o600))

		t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certPath)
		t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", certPath)
		t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", keyPath)
		require.NoError(t, validateOTLPExporterEnv(all), "a readable PEM certificate and a matching key pair pass")

		for _, tc := range []struct{ name, variable, value, named, reason string }{
			{"unreadable CA file", "OTEL_EXPORTER_OTLP_CERTIFICATE", missing, "OTEL_EXPORTER_OTLP_CERTIFICATE", "the file it names cannot be read"},
			{"CA file without a certificate", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", text, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", "holds no PEM certificate"},
			{"unreadable client certificate", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", missing, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "the file it names cannot be read"},
			{"unreadable client key", "OTEL_EXPORTER_OTLP_CLIENT_KEY", missing, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "the file named by OTEL_EXPORTER_OTLP_CLIENT_KEY cannot be read"},
			{"mismatched pair", "OTEL_EXPORTER_OTLP_CLIENT_KEY", text, "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "do not name a valid certificate and key pair"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv(tc.variable, tc.value)
				err := validateOTLPExporterEnv(all)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.named+" cannot be used (")
				assert.Contains(t, err.Error(), tc.reason)
				assert.NotContains(t, err.Error(), dir, "the path stays out of the error")
			})
		}

		t.Run("client pair needs both variables", func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", "")
			t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", missing)
			require.NoError(t, validateOTLPExporterEnv(all), "the exporters load a client certificate only when both variables are set")
		})

		t.Run("log exporter takes the LOGS_ pair only when complete", func(t *testing.T) {
			logsOnly := &Config{logEnabled: true}
			t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", missing)
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CLIENT_CERTIFICATE", certPath)
			err := validateOTLPExporterEnv(logsOnly)
			require.Error(t, err, "a LOGS_ certificate without its key leaves the generic pair in use")
			assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE cannot be used")
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CLIENT_KEY", keyPath)
			require.NoError(t, validateOTLPExporterEnv(logsOnly), "a complete LOGS_ pair shadows the generic one")
			t.Setenv("OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE", certPath)
			t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", missing)
			require.NoError(t, validateOTLPExporterEnv(logsOnly), "a LOGS_ CA file shadows the generic one")
			require.Error(t, validateOTLPExporterEnv(all), "the trace exporter still reads the generic CA file")
		})
	})

	t.Run("SDK variables follow the enabled pillars", func(t *testing.T) {
		tracesOnly := &Config{traceEnabled: true}
		logsOnly := &Config{logEnabled: true}
		pull := &Config{metricsEnabled: true}
		push := &Config{metricsEnabled: true, metricsOTLPEndpoint: "http://collector:4318"}

		t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "huge")
		require.Error(t, validateOTLPExporterEnv(tracesOnly))
		require.NoError(t, validateOTLPExporterEnv(logsOnly), "the span batcher is not built without traces")
		t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "")

		t.Setenv("OTEL_BLRP_MAX_QUEUE_SIZE", "huge")
		require.Error(t, validateOTLPExporterEnv(logsOnly))
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "the log batcher is not built without logs")
		t.Setenv("OTEL_BLRP_MAX_QUEUE_SIZE", "")

		t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "later")
		require.Error(t, validateOTLPExporterEnv(push))
		require.NoError(t, validateOTLPExporterEnv(pull), "the periodic reader exists only on the push path")
		t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "")

		t.Setenv("OTEL_GO_X_CARDINALITY_LIMIT", " lots ")
		require.Error(t, validateOTLPExporterEnv(pull), "the MeterProvider parses the limit on the pull path too")
		require.Error(t, validateOTLPExporterEnv(push))
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "no MeterProvider without the metrics pillar")
		t.Setenv("OTEL_GO_X_CARDINALITY_LIMIT", " 4000 ")
		require.NoError(t, validateOTLPExporterEnv(pull), "the SDK trims the value before parsing it")
		t.Setenv("OTEL_GO_X_CARDINALITY_LIMIT", "")

		t.Setenv("OTEL_ATTRIBUTE_COUNT_LIMIT", "lots")
		t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "64")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "a valid span limit shadows the generic one, which the SDK never reads")
		t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "many")
		t.Setenv("OTEL_ATTRIBUTE_COUNT_LIMIT", "64")
		err := validateOTLPExporterEnv(tracesOnly)
		require.Error(t, err, "a span limit that fails to parse is echoed before the SDK falls back")
		assert.Contains(t, err.Error(), "OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT cannot be used")
		t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "")
		t.Setenv("OTEL_ATTRIBUTE_COUNT_LIMIT", "")

		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "half")
		t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "the argument is read only for a ratio sampler")
		t.Setenv("OTEL_TRACES_SAMPLER", " TraceIDRatio ")
		err = validateOTLPExporterEnv(tracesOnly)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OTEL_TRACES_SAMPLER_ARG cannot be used (it is not a number")
		assert.NotContains(t, err.Error(), "half")
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", "2")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "a ratio out of range is reported by the SDK without the value")
	})

	t.Run("log exporter reads values verbatim", func(t *testing.T) {
		certPath, _ := writeTestKeyPair(t, t.TempDir())
		tracesOnly := &Config{traceEnabled: true}
		logsOnly := &Config{logEnabled: true}

		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", " 10000 ")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "the trace exporter trims the value before parsing it")
		err := validateOTLPExporterEnv(logsOnly)
		require.Error(t, err, "the log exporter parses the value with its whitespace and echoes it")
		assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_TIMEOUT cannot be used")

		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", " ")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "a blank value is unset for the trace exporter")
		require.Error(t, validateOTLPExporterEnv(logsOnly), "a blank value is set, and unparseable, for the log exporter")

		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", " "+certPath+" ")
		require.NoError(t, validateOTLPExporterEnv(tracesOnly), "the trace exporter trims the path")
		err = validateOTLPExporterEnv(logsOnly)
		require.Error(t, err, "the log exporter opens the path with its whitespace")
		assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_CERTIFICATE cannot be used")
		assert.NotContains(t, err.Error(), certPath)
	})
}

// writeTestKeyPair writes a self-signed certificate and its private key as
// PEM files under dir and returns their paths.
func writeTestKeyPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}

// TestValidateConfiguredEndpoints pins that a malformed WithOTLPEndpoint or
// WithMetricsOTLPEndpoint value fails before any exporter is built, with an
// error that names the option and the parser's reason but never the value,
// and that only an endpoint an enabled OTLP exporter uses is checked.
func TestValidateConfiguredEndpoints(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: gosec.G101-1
	const bad = "http://user:secret%zz@collector:4318"

	err := validateConfiguredEndpoints(&Config{traceEnabled: true, otlpEndpoint: bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithOTLPEndpoint is not a valid URL")
	assert.NotContains(t, err.Error(), "secret", "the value stays out of the error")
	assert.NotContains(t, err.Error(), "%zz", "and so does the parser's message, which quotes the escape")

	err = validateConfiguredEndpoints(&Config{traceEnabled: true, otlpEndpoint: "http://collector:BearerSecret"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "BearerSecret", "the parser quotes a non-numeric port in its message")

	err = validateConfiguredEndpoints(&Config{metricsEnabled: true, metricsOTLPEndpoint: bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithMetricsOTLPEndpoint is not a valid URL")
	assert.NotContains(t, err.Error(), "secret")

	require.NoError(t, validateConfiguredEndpoints(&Config{metricsEnabled: true, otlpEndpoint: bad}), "no trace or log exporter uses the OTLP endpoint")
	require.NoError(t, validateConfiguredEndpoints(&Config{traceEnabled: true, logEnabled: true, otlpEndpoint: "http://collector:4318", metricsEnabled: true, metricsOTLPEndpoint: "http://collector:4318"}))
}

// TestValidateConfiguredHeaders pins that a configured header name that is
// not an HTTP token fails Init with an error naming the option but not the
// name, and that token names pass.
func TestValidateConfiguredHeaders(t *testing.T) {
	require.NoError(t, validateConfiguredHeaders(&Config{
		traceEnabled:         true,
		profilingEnabled:     true,
		profilingEndpoint:    "http://pyroscope:4040",
		otlpHeaders:          map[string]string{"authorization": "Bearer x", "x-tenant": "acme"},
		profilingAuthHeaders: map[string]string{"x-api-key": "k"},
	}))

	bad := map[string]string{"Bearer\nSecret": "v"}
	for _, cfg := range []*Config{
		{traceEnabled: true, otlpHeaders: bad},
		{logEnabled: true, otlpHeaders: bad},
		{metricsEnabled: true, metricsOTLPEndpoint: "http://collector:4318", otlpHeaders: bad},
	} {
		err := validateConfiguredHeaders(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WithOTLPHeaders is not a valid HTTP header name")
		assert.NotContains(t, err.Error(), "Secret", "the name stays out of the error")
	}

	err := validateConfiguredHeaders(&Config{profilingEnabled: true, profilingEndpoint: "http://pyroscope:4040", profilingAuthHeaders: map[string]string{"x api key": "k"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithProfilingAuthHeaders is not a valid HTTP header name")

	dormant := &Config{metricsEnabled: true, otlpHeaders: bad, profilingEndpoint: "http://pyroscope:4040", profilingAuthHeaders: map[string]string{"x api key": "k"}}
	require.NoError(t, validateConfiguredHeaders(dormant), "no OTLP exporter on the pull path and no profiler without the toggle, so neither map is sent")
	require.NoError(t, validateConfiguredHeaders(&Config{profilingEnabled: true, profilingAuthHeaders: map[string]string{"x api key": "k"}}), "profiling without an endpoint does not start")
}

// TestDiagnosticSecrets_EscapedForms pins that a configured value whose
// %q rendering differs from the value is listed in both forms, since
// net/http quotes a header name that way in its error.
func TestDiagnosticSecrets_EscapedForms(t *testing.T) {
	secrets := diagnosticSecrets(&Config{otlpHeaders: map[string]string{"x-token": "Bearer\nSecret"}})
	assert.Contains(t, secrets, "Bearer\nSecret")
	assert.Contains(t, secrets, `Bearer\nSecret`)
	assert.Equal(t, 1, countOf(secrets, "x-token"), "a value the escaping leaves alone is listed once")
}

func countOf(list []string, v string) int {
	n := 0
	for _, s := range list {
		if s == v {
			n++
		}
	}
	return n
}

// derefError dereferences its receiver in Error, so a typed nil *derefError
// panics when rendered the ordinary way.
type derefError struct{ msg string }

func (e *derefError) Error() string { return e.msg }

// panicError panics on any receiver.
type panicError struct{}

func (panicError) Error() string { panic("no") }

// TestOTelErrorHandler_SurvivesBrokenErrors pins that a typed nil error and
// an Error method that panics are rendered as placeholders rather than
// taking the process down, and that a live error still renders.
func TestOTelErrorHandler_SurvivesBrokenErrors(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))

	var typed *derefError
	assert.NotPanics(t, func() { h.Handle(typed) })
	assert.Contains(t, buf.String(), "<nil *o11y.derefError>")

	buf.Reset()
	assert.NotPanics(t, func() { h.Handle(panicError{}) })
	assert.Contains(t, buf.String(), "[omitted: o11y.panicError panicked while rendering]")

	buf.Reset()
	h.Handle(&derefError{msg: "still rendered"})
	assert.Contains(t, buf.String(), "still rendered")
}

// TestOTelErrorHandler_NilIsIgnored checks nil errors and a nil logger are safe.
func TestOTelErrorHandler_NilIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	h := newOTelErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)))
	assert.NotPanics(t, func() { h.Handle(nil) })
	assert.Empty(t, buf.String())

	assert.NotPanics(t, func() { newOTelErrorHandler(nil).Handle(errors.New("x")) })
}

// TestShutdownBudget_SharesDeadlineAcrossClosers checks the share each
// closer gets: an even split of the time left, the whole remainder for the
// last closer, and ctx itself when there is no deadline or it has passed.
func TestShutdownBudget_SharesDeadlineAcrossClosers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()

	first, cancelFirst := shutdownBudget(ctx, 4)
	defer cancelFirst()
	firstDeadline, ok := first.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(250*time.Millisecond), firstDeadline, 50*time.Millisecond)

	last, cancelLast := shutdownBudget(ctx, 1)
	defer cancelLast()
	lastDeadline, _ := last.Deadline()
	assert.Equal(t, deadline, lastDeadline, "the last closer gets everything that is left")

	noDeadline, cancelNone := shutdownBudget(context.Background(), 3)
	defer cancelNone()
	_, ok = noDeadline.Deadline()
	assert.False(t, ok, "no deadline to share")

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	past, cancelPast := shutdownBudget(expired, 3)
	defer cancelPast()
	assert.ErrorIs(t, past.Err(), context.DeadlineExceeded, "an expired deadline is passed through")
}

// TestShutdown_LaterClosersKeepALiveContext is the failure Codex described:
// a closer that blocks until its context is done must not leave the ones
// after it, the meter provider's final collection, with a context that is
// already cancelled.
func TestShutdown_LaterClosersKeepALiveContext(t *testing.T) {
	var sawLive bool
	sdk := &SDK{
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		shutdowns: []func(context.Context) error{
			func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			func(ctx context.Context) error {
				sawLive = ctx.Err() == nil
				return nil
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := sdk.Shutdown(ctx)

	assert.ErrorIs(t, err, context.DeadlineExceeded, "the slow closer's own timeout is reported")
	assert.True(t, sawLive, "the closer after a slow one still ran with a live context")
	assert.NoError(t, ctx.Err(), "the caller's deadline itself was not exhausted")
}

// TestShutdown_RedactsCredentialsFromTheExporterError pins that neither the
// record Shutdown writes nor the error it returns carries the endpoint's
// credentials.
//
// net/http masks the password in the URL it reports and keeps the username:
//
//	Post "http://alice:***@collector:4318/v1/traces": dial tcp: …
//
// so an "@" survives into the message, which the SDK's own rule forbids. The
// returned error matters as much as the logged one: the guide's pattern is
// slog.Any("error", obs.Shutdown(ctx)) in the caller's defer, so leaving the
// username on the return value would move the leak rather than remove it.
func TestShutdown_RedactsCredentialsFromTheExporterError(t *testing.T) {
	var buf bytes.Buffer
	exportErr := fmt.Errorf(`Post "http://alice:***@collector:4318/v1/traces": dial tcp: i/o timeout`) //nolint:err113
	sdk := &SDK{
		Logger:              slog.New(slog.NewTextHandler(&buf, nil)),
		diagnosticEndpoints: []string{"http://alice:s3cretpw@collector:4318"},
		diagnosticSecrets:   []string{"glc_token"},
		shutdowns: []func(context.Context) error{
			func(context.Context) error { return exportErr },
			func(context.Context) error {
				return fmt.Errorf("metric exporter: header glc_token rejected") //nolint:err113
			},
		},
	}

	err := sdk.Shutdown(context.Background())

	require.Error(t, err)
	for _, where := range map[string]string{"the logged record": buf.String(), "the returned error": err.Error()} {
		assert.NotContains(t, where, "alice", "the endpoint username must not survive")
		assert.NotContains(t, where, "glc_token", "nor a configured header value")
		assert.Contains(t, where, "collector:4318", "the collector still has to be identifiable")
	}

	assert.ErrorIs(t, err, exportErr,
		"redaction rewrites the message, so the caller can still match the exporter's own error")
}

// TestShutdown_LeavesACleanErrorAlone pins that an error with nothing to
// redact is returned as it stands, so a caller comparing error values — not
// only matching with errors.Is — still sees the exporter's own error.
func TestShutdown_LeavesACleanErrorAlone(t *testing.T) {
	exportErr := errors.New("trace exporter: context deadline exceeded")
	sdk := &SDK{
		Logger:              slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		diagnosticEndpoints: []string{"http://collector:4318"},
		shutdowns:           []func(context.Context) error{func(context.Context) error { return exportErr }},
	}

	err := sdk.Shutdown(context.Background())

	require.Error(t, err)
	var joined interface{ Unwrap() []error }
	require.ErrorAs(t, err, &joined, "Shutdown joins its closers' errors")
	require.Len(t, joined.Unwrap(), 1)
	assert.Equal(t, exportErr, joined.Unwrap()[0],
		"nothing needed redacting, so the exporter's own error is passed through unwrapped")
	assert.Equal(t, exportErr.Error(), err.Error())
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

	order = nil
	seq = shutdownSequence(nil, closer("traces"), nil, nil, nil)
	require.Len(t, seq, 1, "disabled pillars neither run nor count towards the deadline share")
	for _, fn := range seq {
		require.NoError(t, fn(context.Background()))
	}
	assert.Equal(t, []string{"traces"}, order)
}

// TestShutdown_DisabledPillarsDoNotShareTheDeadline checks the case Codex
// raised: with tracing the only enabled pillar, the tracer's closer must get
// the whole deadline rather than a quarter of it.
func TestShutdown_DisabledPillarsDoNotShareTheDeadline(t *testing.T) {
	var got time.Time
	sdk := &SDK{
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		shutdowns: shutdownSequence(nil, func(ctx context.Context) error {
			got, _ = ctx.Deadline()
			return nil
		}, nil, nil, nil),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := ctx.Deadline()
	require.NoError(t, sdk.Shutdown(ctx))
	assert.Equal(t, want, got, "the only closer runs under the caller's own deadline")
}

// TestDiagnosticSecrets_WireForm pins that a configured header value is listed
// as net/http sends it as well as as it was written.
//
// Header.Set stores the configured string untouched and the write path trims
// surrounding whitespace, so a server or an exporter that reports the header it
// received names a string redact.Secrets would not otherwise match.
func TestDiagnosticSecrets_WireForm(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const padded = "\tBearer configured-secret\t"

	secrets := diagnosticSecrets(&Config{otlpHeaders: map[string]string{"x-api-key": padded}})

	assert.Contains(t, secrets, "Bearer configured-secret", "the form that goes on the wire")
	assert.Contains(t, secrets, padded, "and the form it was configured as")
}

// TestDiagnosticSecrets_ExporterNormalizedEnvValue pins the form the pinned
// exporter actually sends for an environment-configured header.
//
// stringToHeader unescapes the value and then applies strings.TrimSpace, in
// that order — and strings.TrimSpace takes Unicode spaces net/http would have
// kept, so HeaderWireValue's ASCII trim does not produce it either. The list
// therefore held the encoded form and the decoded-but-padded form, and not the
// one that goes out. The exporter puts a non-2xx response body into the error
// it returns, so a collector echoing the header it received names exactly that.
func TestDiagnosticSecrets_ExporterNormalizedEnvValue(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const configured = "authorization=%C2%A0BearerSecret%C2%A0"
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", configured)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")

	// What the exporter sends, derived the way it derives it rather than
	// spelled out, so this fails if the pinned parser ever changes.
	_, rawValue, ok := strings.Cut(configured, "=")
	require.True(t, ok)
	unescaped, err := url.PathUnescape(rawValue)
	require.NoError(t, err)
	sent := strings.TrimSpace(unescaped)
	require.Equal(t, "BearerSecret", sent, "the premise: neither the configured nor the ASCII-trimmed form")

	assert.Contains(t, diagnosticSecrets(&Config{}), sent)
}

// TestDiagnosticSecrets_CanonicalHeaderName pins the form net/http stores a
// configured header name under. Header.Set keys by
// textproto.CanonicalMIMEHeaderKey, so a credential pasted in as a name
// reaches the collector canonicalized.
func TestDiagnosticSecrets_CanonicalHeaderName(t *testing.T) {
	// #nosec G101 -- fabricated fixture header name, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const pastedAsAName = "x-secret-glc-token"

	secrets := diagnosticSecrets(&Config{otlpHeaders: map[string]string{pastedAsAName: "1"}})

	assert.Contains(t, secrets, pastedAsAName, "the configured spelling")
	assert.Contains(t, secrets, "X-Secret-Glc-Token", "and the one that goes on the wire")
}

// TestDiagnosticSecrets_CoversTheJSONForm pins the same for the OTLP header
// list: a collector reporting a rejected header in a JSON body writes the
// encoding/json rendering of it, and the exporter puts that body into the
// error otelErrorHandler records.
func TestDiagnosticSecrets_CoversTheJSONForm(t *testing.T) {
	// #nosec G101 -- fabricated fixture header value, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const token = "Bearer a<b&c>d"

	secrets := diagnosticSecrets(&Config{otlpHeaders: map[string]string{"x-api-key": token}})

	assert.Contains(t, secrets, token, "the configured form")
	assert.Contains(t, secrets, redact.JSONEscaped(token), "and the form a JSON error body holds")
}

// TestDiagnosticSecrets_EnvFragmentsGetValueFormsToo pins that an
// environment-configured header is covered on the value side as well as the
// name side.
//
// The two sets overlap only in the fragment itself. A Cookie value reaches the
// collector rejoined with "; " by HTTP/2, and a value with surrounding
// whitespace reaches it trimmed — neither of which HeaderNameForms produces.
// A refactor that routed this loop through the name set alone dropped both,
// while the comment above it still claimed "both sets of forms".
func TestDiagnosticSecrets_EnvFragmentsGetValueFormsToo(t *testing.T) {
	// #nosec G101 -- fabricated fixture header, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const configured = "Cookie=session=x;tenant=t0ken"
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", configured)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")

	_, value, ok := strings.Cut(configured, "=")
	require.True(t, ok)
	onTheWire := redact.CookieWireValue(value)
	require.NotEqual(t, value, onTheWire, "the premise: HTTP/2 rewrites this value")

	secrets := diagnosticSecrets(&Config{})

	assert.Contains(t, secrets, value, "the configured form")
	assert.Contains(t, secrets, onTheWire, "and the form an HTTP/2 collector receives")
}

// TestDiagnosticSecrets_EnvCookieWithAnEscapedSeparator pins the harder
// ordering: the separator has to be unescaped before the Cookie normalization
// can see it at all.
//
// The exporter unescapes an environment value and then trims it, so a Cookie
// written as "session=T%3Btenant=x" is sent with a real ";" — and an HTTP/2
// collector then receives it rejoined with "; ". The list only reaches that
// spelling because headerEnvForms decodes first and each decoded form is then
// expanded through HeaderValueForms; a value-forms pass over the raw fragment
// alone would find no separator to normalize.
func TestDiagnosticSecrets_EnvCookieWithAnEscapedSeparator(t *testing.T) {
	// #nosec G101 -- fabricated fixture header, not a live credential
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const configured = "Cookie=session=S3cretToken%3Btenant=x"
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", configured)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")

	_, escaped, ok := strings.Cut(configured, "=")
	require.True(t, ok)
	require.NotContains(t, escaped, ";", "the premise: the raw fragment has no separator")
	decoded, err := url.PathUnescape(escaped)
	require.NoError(t, err)
	onTheWire := redact.CookieWireValue(decoded)
	require.Equal(t, "session=S3cretToken; tenant=x", onTheWire)

	assert.Contains(t, diagnosticSecrets(&Config{}), onTheWire)
}

// TestDiagnosticSecrets_CoversEndpointDerivedBasicAuth pins that the OTLP and
// profiling endpoints get the same treatment authHeaderSecrets gives the
// profiling one.
//
// http.Client derives "Authorization: Basic base64(user:pass)" from an
// endpoint's userinfo, and both exporters put a non-2xx response body into the
// error they return. The base64 holds neither half as a substring, so nothing
// else in this list would match a collector that echoed the header back.
func TestDiagnosticSecrets_CoversEndpointDerivedBasicAuth(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoints, not live credentials
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const traces = "http://alice:tr4cePass@collector:4318"
	// #nosec G101 -- fabricated fixture endpoints, not live credentials
	// nosemgrep: hardcoded-credential-literal,gosec.G101-1
	const profiles = "http://bob:pr0filePass@pyroscope:4040"

	secrets := diagnosticSecrets(&Config{otlpEndpoint: traces, profilingEndpoint: profiles})

	for _, endpoint := range []string{traces, profiles} {
		derived := redact.BasicAuthHeader(endpoint)
		require.NotEmpty(t, derived)
		assert.Contains(t, secrets, derived)
		assert.Contains(t, secrets, strings.TrimPrefix(derived, "Basic "))
	}
	for _, s := range secrets {
		assert.NotContains(t, s, "tr4cePass")
		assert.NotContains(t, s, "pr0filePass")
	}
}
