package o11y_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flywindy/o11y"
	"github.com/flywindy/o11y/internal/testutil"
)

// identityOpts are the required options without WithOTLPEndpoint, so the
// standard endpoint variables decide where traces and logs go.
func identityOpts() []o11y.Option {
	return []o11y.Option{
		o11y.WithServiceName("test-svc"),
		o11y.WithServiceVersion("0.1.0"),
		o11y.WithEnvironment("development"),
		o11y.WithServiceNamespace("platform"),
		o11y.WithMetricsAddr("127.0.0.1:0"),
	}
}

// emitAndFlush records one span and one log record and flushes both.
func emitAndFlush(t *testing.T, sdk *o11y.SDK) {
	t.Helper()
	_, span := sdk.Tracer("endpoint-test").Start(t.Context(), "op")
	span.End()
	sdk.Logger.InfoContext(t.Context(), "endpoint test")
	require.NoError(t, sdk.ForceFlush(t.Context()))
}

func requestPaths(srv *testutil.CapturingOTLPServer) map[string]bool {
	paths := map[string]bool{}
	for _, r := range srv.Requests() {
		paths[r.Path] = true
	}
	return paths
}

// TestInit_HonoursOTLPEndpointEnv pins the OTel specification's precedence
// when WithOTLPEndpoint is not given, and that both signals complete a
// variable's path alike: a signal variable is used as given minus a
// trailing slash, with the default path when it has none; the generic one
// is a base URL the signal path is appended to.
func TestInit_HonoursOTLPEndpointEnv(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       map[string]string // values are suffixes appended to the server URL
		wantPaths []string
	}{
		{
			name:      "generic variable",
			env:       map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": ""},
			wantPaths: []string{"/v1/traces", "/v1/logs"},
		},
		{
			name:      "generic variable with a base path",
			env:       map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "/otlp"},
			wantPaths: []string{"/otlp/v1/traces", "/otlp/v1/logs"},
		},
		{
			name:      "generic variable with a trailing slash",
			env:       map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "/"},
			wantPaths: []string{"/v1/traces", "/v1/logs"},
		},
		{
			name: "signal variables without a path get the default path",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "",
			},
			wantPaths: []string{"/v1/traces", "/v1/logs"},
		},
		{
			name: "signal variables with only a slash get the default path",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "/",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "/",
			},
			wantPaths: []string{"/v1/traces", "/v1/logs"},
		},
		{
			name: "signal variables lose a trailing slash alike",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "/custom/traces/",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "/custom/logs/",
			},
			wantPaths: []string{"/custom/traces", "/custom/logs"},
		},
		{
			name: "signal variables with surrounding whitespace",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "/custom/traces\n",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "/custom/logs\n",
			},
			wantPaths: []string{"/custom/traces", "/custom/logs"},
		},
		{
			name: "signal variables over the generic one",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":        "/ignored",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "/custom/traces",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "/custom/logs",
			},
			wantPaths: []string{"/custom/traces", "/custom/logs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewCapturingOTLPServer(t)
			for k, suffix := range tc.env {
				t.Setenv(k, srv.URL+suffix)
			}
			sdk, err := o11y.Init(t.Context(), identityOpts()...)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })

			emitAndFlush(t, sdk)
			paths := requestPaths(srv)
			for _, want := range tc.wantPaths {
				assert.True(t, paths[want], "expected a request to %s, got %v", want, paths)
			}
		})
	}
}

// TestInit_EnvEndpointSchemeDecidesInsecure pins that an http:// endpoint
// variable is sent to in plaintext for both signals whatever
// OTEL_EXPORTER_OTLP_INSECURE says, as a WithOTLPEndpoint value is: left to
// read the variables, the trace exporter would let INSECURE=false turn it
// into https (and INSECURE=true turn an https one into plaintext) while the
// log exporter followed the scheme.
func TestInit_EnvEndpointSchemeDecidesInsecure(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "false")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "false")
	sdk, err := o11y.Init(t.Context(), identityOpts()...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })

	emitAndFlush(t, sdk)
	paths := requestPaths(srv)
	assert.True(t, paths["/v1/traces"] && paths["/v1/logs"], "both signals reach the http:// endpoint, got %v", paths)
}

// TestInit_OTLPEndpointOptionWinsOverEnv pins that WithOTLPEndpoint always
// wins over the variables.
func TestInit_OTLPEndpointOptionWinsOverEnv(t *testing.T) {
	configured := testutil.NewCapturingOTLPServer(t)
	fromEnv := testutil.NewCapturingOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", fromEnv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", fromEnv.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", fromEnv.URL+"/v1/logs")

	sdk, err := o11y.Init(t.Context(), append(identityOpts(), o11y.WithOTLPEndpoint(configured.URL))...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })

	emitAndFlush(t, sdk)
	paths := requestPaths(configured)
	assert.True(t, paths["/v1/traces"] && paths["/v1/logs"], "the option's endpoint gets traces and logs, got %v", paths)
	assert.Empty(t, fromEnv.Requests(), "the variables are not used when the option is given")
}

// TestInit_RejectsUnusableEndpoint pins that an endpoint without an
// http(s) scheme and a host fails Init, naming where it came from and not
// repeating the value, whether it came from the option or from a variable
// the exporter would send to.
func TestInit_RejectsUnusableEndpoint(t *testing.T) {
	const noScheme, noHost = "no http:// or https:// scheme", "no host"
	for _, tc := range []struct {
		name     string
		opts     []o11y.Option
		env      map[string]string
		wantName string
		want     string
		// echo is whether the error may say the value would be echoed:
		// true only for a variable the trace exporter parses itself and
		// cannot, false for what only Init reads.
		echo bool
	}{
		{name: "option without a scheme", opts: []o11y.Option{o11y.WithOTLPEndpoint("collector.secret-host:4318")}, wantName: "WithOTLPEndpoint", want: noScheme},
		{name: "option that only lacks a scheme", opts: []o11y.Option{o11y.WithOTLPEndpoint("10.0.0.7:4318")}, wantName: "WithOTLPEndpoint", want: noScheme},
		{name: "option with another scheme", opts: []o11y.Option{o11y.WithOTLPEndpoint("grpc://collector.secret-host:4317")}, wantName: "WithOTLPEndpoint", want: noScheme},
		{name: "option missing a slash", opts: []o11y.Option{o11y.WithOTLPEndpoint("http:/collector.secret-host:4318")}, wantName: "WithOTLPEndpoint", want: noHost},
		{name: "generic variable without a scheme", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector.secret-host:4318"}, wantName: "OTEL_EXPORTER_OTLP_ENDPOINT", want: noScheme},
		{name: "generic variable that does not parse, traces on", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "10.0.0.7:4318"}, wantName: "OTEL_EXPORTER_OTLP_ENDPOINT", want: "it is not a valid URL", echo: true},
		{
			name:     "generic variable without a scheme, traces off",
			opts:     []o11y.Option{o11y.WithTraceEnabled(false)},
			env:      map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "10.0.0.7:4318"},
			wantName: "OTEL_EXPORTER_OTLP_ENDPOINT", want: noScheme,
		},
		{
			name:     "trace variable without a scheme, logs off",
			opts:     []o11y.Option{o11y.WithLogEnabled(false)},
			env:      map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "collector.secret-host:4318"},
			wantName: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", want: noScheme,
		},
		{name: "log variable without a host", env: map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http:///v1/logs"}, wantName: "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", want: noHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			sdk, err := o11y.Init(t.Context(), append(identityOpts(), tc.opts...)...)
			if err == nil {
				_ = sdk.Shutdown(context.Background())
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantName)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "secret-host", "the value is not repeated")
			assert.NotContains(t, err.Error(), "10.0.0.7", "the value is not repeated")
			if !tc.echo {
				assert.NotContains(t, err.Error(), "global logger", "only a value an exporter parses itself would be echoed")
			}
		})
	}
}

// TestInit_IgnoresAnOverriddenSchemelessVariable pins that a scheme-less
// variable the option overrides does not fail Init: no exporter sends to it.
func TestInit_IgnoresAnOverriddenSchemelessVariable(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4318")
	sdk, err := o11y.Init(t.Context(), append(identityOpts(), o11y.WithOTLPEndpoint(srv.URL))...)
	require.NoError(t, err)
	require.NoError(t, sdk.Shutdown(context.Background()))
}

// TestInit_RejectsGRPCEndpointVariable pins that an endpoint variable a
// protocol variable marks as gRPC fails Init, naming both variables, since
// the SDK's OTLP/HTTP requests would be refused there; with WithOTLPEndpoint
// the variables are not used and Init succeeds.
func TestInit_RejectsGRPCEndpointVariable(t *testing.T) {
	srv := testutil.NewCapturingOTLPServer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.secret-host:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")

	sdk, err := o11y.Init(t.Context(), identityOpts()...)
	if err == nil {
		_ = sdk.Shutdown(context.Background())
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT cannot be used as the trace endpoint")
	assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL asks for grpc")
	assert.NotContains(t, err.Error(), "secret-host")

	sdk, err = o11y.Init(t.Context(), append(identityOpts(), o11y.WithOTLPEndpoint(srv.URL))...)
	require.NoError(t, err)
	require.NoError(t, sdk.Shutdown(context.Background()))
}
