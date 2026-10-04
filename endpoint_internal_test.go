package o11y

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveEndpoint pins which endpoint Init hands each exporter: the
// option when given, then the signal variable, then the generic variable
// with the signal's path appended, then the default; variables are read
// trimmed and completed with one path rule for both signals.
func TestResolveEndpoint(t *testing.T) {
	const def = "http://localhost:4318"
	for _, tc := range []struct {
		name                   string
		options                []string
		env                    map[string]string
		wantTrace, wantLog     string
		wantTraceSrc, wantLogS string
	}{
		{name: "nothing set", wantTrace: def, wantLog: def},
		{
			name:      "option over every variable",
			options:   []string{"http://opt:4318"},
			env:       map[string]string{envOTLPEndpoint: "http://env:4318", envOTLPTracesEndpoint: "http://env:4318/t", envOTLPLogsEndpoint: "http://env:4318/l"},
			wantTrace: "http://opt:4318", wantLog: "http://opt:4318",
			wantTraceSrc: "WithOTLPEndpoint", wantLogS: "WithOTLPEndpoint",
		},
		{name: "a blank option undoes an earlier one", options: []string{"http://opt:4318", " \n"}, wantTrace: def, wantLog: def},
		{
			name:      "the option is trimmed",
			options:   []string{" http://opt:4318\n"},
			wantTrace: "http://opt:4318", wantLog: "http://opt:4318",
			wantTraceSrc: "WithOTLPEndpoint", wantLogS: "WithOTLPEndpoint",
		},
		{name: "an empty option undoes an earlier one", options: []string{"http://opt:4318", ""}, wantTrace: def, wantLog: def},
		{
			name:      "an empty option leaves the variables in charge",
			options:   []string{"http://opt:4318", ""},
			env:       map[string]string{envOTLPEndpoint: "http://env:4318"},
			wantTrace: "http://env:4318/v1/traces", wantLog: "http://env:4318/v1/logs",
			wantTraceSrc: envOTLPEndpoint, wantLogS: envOTLPEndpoint,
		},
		{
			name:      "generic variable is a base URL",
			env:       map[string]string{envOTLPEndpoint: "http://env:4318"},
			wantTrace: "http://env:4318/v1/traces", wantLog: "http://env:4318/v1/logs",
			wantTraceSrc: envOTLPEndpoint, wantLogS: envOTLPEndpoint,
		},
		{
			name:      "generic variable with a trailing slash",
			env:       map[string]string{envOTLPEndpoint: "http://env:4318/"},
			wantTrace: "http://env:4318/v1/traces", wantLog: "http://env:4318/v1/logs",
			wantTraceSrc: envOTLPEndpoint, wantLogS: envOTLPEndpoint,
		},
		{
			name:      "generic variable with a base path and a query",
			env:       map[string]string{envOTLPEndpoint: "https://env/otlp/?k=v"},
			wantTrace: "https://env/otlp/v1/traces?k=v", wantLog: "https://env/otlp/v1/logs?k=v",
			wantTraceSrc: envOTLPEndpoint, wantLogS: envOTLPEndpoint,
		},
		{
			name:      "signal variable over the generic one",
			env:       map[string]string{envOTLPEndpoint: "http://env:4318", envOTLPTracesEndpoint: "http://sig:4318/t"},
			wantTrace: "http://sig:4318/t", wantLog: "http://env:4318/v1/logs",
			wantTraceSrc: envOTLPTracesEndpoint, wantLogS: envOTLPEndpoint,
		},
		{
			name:      "signal variables without a path get the default path",
			env:       map[string]string{envOTLPTracesEndpoint: "http://sig:4318", envOTLPLogsEndpoint: "http://sig:4318"},
			wantTrace: "http://sig:4318/v1/traces", wantLog: "http://sig:4318/v1/logs",
			wantTraceSrc: envOTLPTracesEndpoint, wantLogS: envOTLPLogsEndpoint,
		},
		{
			name:      "signal variables with only a slash get the default path",
			env:       map[string]string{envOTLPTracesEndpoint: "http://sig:4318/", envOTLPLogsEndpoint: "http://sig:4318/"},
			wantTrace: "http://sig:4318/v1/traces", wantLog: "http://sig:4318/v1/logs",
			wantTraceSrc: envOTLPTracesEndpoint, wantLogS: envOTLPLogsEndpoint,
		},
		{
			name:      "signal variables lose a trailing slash alike",
			env:       map[string]string{envOTLPTracesEndpoint: "http://sig:4318/custom/", envOTLPLogsEndpoint: "http://sig:4318/custom/"},
			wantTrace: "http://sig:4318/custom", wantLog: "http://sig:4318/custom",
			wantTraceSrc: envOTLPTracesEndpoint, wantLogS: envOTLPLogsEndpoint,
		},
		{
			name:      "log variable only",
			env:       map[string]string{envOTLPLogsEndpoint: "http://sig:4318/l"},
			wantTrace: def, wantLog: "http://sig:4318/l",
			wantLogS: envOTLPLogsEndpoint,
		},
		{
			name:      "values are trimmed for both signals",
			env:       map[string]string{envOTLPTracesEndpoint: " http://sig:4318/t\n", envOTLPLogsEndpoint: "http://sig:4318/l\n"},
			wantTrace: "http://sig:4318/t", wantLog: "http://sig:4318/l",
			wantTraceSrc: envOTLPTracesEndpoint, wantLogS: envOTLPLogsEndpoint,
		},
		{name: "a blank variable is unset", env: map[string]string{envOTLPEndpoint: " \n"}, wantTrace: def, wantLog: def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := defaultConfig()
			for _, o := range tc.options {
				WithOTLPEndpoint(o)(cfg)
			}
			resolveOTLPEndpoints(cfg)
			got := cfg.resolvedOTLP
			assert.Equal(t, tc.wantTrace, got.trace.url)
			assert.Equal(t, tc.wantTraceSrc, got.trace.source)
			assert.Empty(t, got.trace.problem)
			assert.Empty(t, got.trace.grpcVar)
			assert.Equal(t, tc.wantLog, got.log.url)
			assert.Equal(t, tc.wantLogS, got.log.source)
			assert.Empty(t, got.log.problem)
			assert.Empty(t, got.log.grpcVar)
		})
	}
}

// TestResolveEndpoint_GRPCProtocol pins when an endpoint variable is marked
// as a gRPC endpoint: the signal's PROTOCOL variable decides, else the
// generic one, and only for an endpoint that came from a variable.
func TestResolveEndpoint_GRPCProtocol(t *testing.T) {
	const tracesProtocol, logsProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"
	for _, tc := range []struct {
		name               string
		option             string
		env                map[string]string
		wantTrace, wantLog string
	}{
		{
			name:      "generic protocol",
			env:       map[string]string{envOTLPEndpoint: "http://c:4317", envOTLPProtocol: "grpc"},
			wantTrace: envOTLPProtocol, wantLog: envOTLPProtocol,
		},
		{
			name:      "signal protocol over the generic one",
			env:       map[string]string{envOTLPEndpoint: "http://c:4317", envOTLPProtocol: "grpc", tracesProtocol: "http/protobuf"},
			wantTrace: "", wantLog: envOTLPProtocol,
		},
		{
			name:      "signal protocol alone, trimmed and in any case",
			env:       map[string]string{envOTLPEndpoint: "http://c:4317", logsProtocol: " GRPC\n"},
			wantTrace: "", wantLog: logsProtocol,
		},
		{name: "http protocols", env: map[string]string{envOTLPEndpoint: "http://c:4318", envOTLPProtocol: "http/json", logsProtocol: "http/protobuf"}},
		{name: "the option is not a variable", option: "http://c:4318", env: map[string]string{envOTLPEndpoint: "http://c:4317", envOTLPProtocol: "grpc"}},
		{name: "nor is the default", env: map[string]string{envOTLPProtocol: "grpc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := defaultConfig()
			WithOTLPEndpoint(tc.option)(cfg)
			resolveOTLPEndpoints(cfg)
			assert.Equal(t, tc.wantTrace, cfg.resolvedOTLP.trace.grpcVar)
			assert.Equal(t, tc.wantLog, cfg.resolvedOTLP.log.grpcVar)
		})
	}
}

// TestEndpointProblem pins what an OTLP/HTTP endpoint must be: an http(s)
// URL with a host.
func TestEndpointProblem(t *testing.T) {
	for v, want := range map[string]string{
		"http://collector:4318":      "",
		"HTTPS://collector/otlp":     "",
		"http://[::1]:4318":          "",
		"":                           "it has no http:// or https:// scheme",
		"collector:4318":             "it has no http:// or https:// scheme",
		"10.0.0.7:4318":              "it has no http:// or https:// scheme",
		"grpc://collector:4317":      "it has no http:// or https:// scheme",
		"http://collector:4318/%zz":  "it is not a valid URL",
		"http:///v1/logs":            "it has no host",
		"http://:4318":               "",
		"http://alice@:4318/v1/logs": "",
		"http:/collector:4318":       "it has no host",
		"https:collector":            "it has no host",
	} {
		assert.Equal(t, want, endpointProblem(v), "endpointProblem(%q)", v)
	}
}

// TestDiagnosticsRedactEnvEndpoints pins that credentials in an endpoint
// variable an exporter sends to are redacted like a configured endpoint's:
// the variable and the URL derived from it are in the endpoint list and its
// Basic header in the secrets. A variable no exporter sends to is left out,
// so a stale one cannot blank out the endpoint in use.
func TestDiagnosticsRedactEnvEndpoints(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: gosec.G101-1
	const endpoint = "http://alice:s3cretpw@collector:4318"
	t.Setenv(envOTLPEndpoint, endpoint)
	t.Setenv(envOTLPLogsEndpoint, "http://stale:4318/v1/logs")
	cfg := defaultConfig()
	cfg.traceEnabled, cfg.logEnabled = true, false
	resolveOTLPEndpoints(cfg)

	endpoints := diagnosticEndpoints(cfg)
	assert.Contains(t, endpoints, endpoint)
	assert.Contains(t, endpoints, endpoint+"/v1/traces")
	assert.NotContains(t, endpoints, "http://stale:4318/v1/logs", "logs are off, so nothing sends to their variable")
	assert.NotContains(t, endpoints, defaultOTLPEndpoint, "the variable decides traces, so the default is not sent to")

	cfg.logEnabled = true
	t.Setenv(envOTLPLogsEndpoint, "")
	t.Setenv(envOTLPEndpoint, "")
	resolveOTLPEndpoints(cfg)
	assert.Contains(t, diagnosticEndpoints(cfg), defaultOTLPEndpoint, "with no variable, the default is sent to")
	cfg.logEnabled = false
	t.Setenv(envOTLPEndpoint, endpoint)
	resolveOTLPEndpoints(cfg)
	assert.Contains(t, diagnosticSecrets(cfg), "YWxpY2U6czNjcmV0cHc=", "the Basic credential derived from the variable's userinfo")

	WithOTLPEndpoint("http://10.0.0.7:4318")(cfg)
	resolveOTLPEndpoints(cfg)
	endpoints = diagnosticEndpoints(cfg)
	assert.NotContains(t, endpoints, endpoint, "a variable the option overrides is not sent to")
	assert.NotContains(t, endpoints, endpoint+"/v1/traces")
}

// TestInit_RedactsEnvEndpointURL pins that Init hands the diagnostics an
// endpoint variable an exporter sends to, so its URL is redacted in the
// records ErrorHandler and Logr write.
func TestInit_RedactsEnvEndpointURL(t *testing.T) {
	// #nosec G101 -- fabricated fixture endpoint, not a live credential
	// nosemgrep: gosec.G101-1
	const endpoint = "http://alice:s3cretpw@127.0.0.1:1"
	t.Setenv(envOTLPEndpoint, endpoint)
	sdk, err := Init(t.Context(),
		WithServiceName("svc"), WithServiceVersion("1.0.0"),
		WithServiceNamespace("platform"), WithEnvironment("testing"),
		WithMetricsAddr("127.0.0.1:0"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })
	assert.Contains(t, sdk.diagnosticEndpoints, endpoint)
	assert.Contains(t, sdk.diagnosticEndpoints, endpoint+"/v1/traces")
}

// TestWithMetricsOTLPEndpoint_Trims pins that the metrics option reads its
// value as WithOTLPEndpoint does: trimmed, and blank meaning not given (the
// pull path), so one value can feed both options.
func TestWithMetricsOTLPEndpoint_Trims(t *testing.T) {
	cfg := defaultConfig()
	WithMetricsOTLPEndpoint(" http://c:4318\n")(cfg)
	assert.Equal(t, "http://c:4318", cfg.metricsOTLPEndpoint)
	WithMetricsOTLPEndpoint(" \n")(cfg)
	assert.Empty(t, cfg.metricsOTLPEndpoint)
}
