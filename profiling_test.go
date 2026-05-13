package o11y

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/grafana/pyroscope-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProfiler struct {
	stopErr error
	stopped bool
}

func (f *fakeProfiler) Stop() error {
	f.stopped = true
	return f.stopErr
}

func withFakePyroscopeStart(t *testing.T, fn func(pyroscope.Config) (profilerHandle, error)) {
	t.Helper()

	oldStart := pyroscopeStart
	pyroscopeStart = fn
	t.Cleanup(func() {
		pyroscopeStart = oldStart
		profilerMu.Lock()
		profilerStarted = false
		profilerMu.Unlock()
	})
}

func newFakeOTLPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func profilingOpts(srvURL string, extra ...Option) []Option {
	opts := []Option{
		WithServiceName("profiled-svc"),
		WithServiceVersion("1.2.3"),
		WithEnvironment("production"),
		WithServiceNamespace("platform"),
		WithMetricsAddr("127.0.0.1:0"),
		WithOTLPEndpoint(srvURL),
		WithProfilingEndpoint("http://alloy.infra.svc.cluster.local:4040"),
	}
	return append(opts, extra...)
}

func TestInit_ProfilingStartsPyroscopeWithResourceTagsAndHeaders(t *testing.T) {
	srv := newFakeOTLPServer(t)
	headers := map[string]string{
		"Authorization": "Bearer original",
		"X-Scope-OrgID": "tenant-a",
	}

	var captured pyroscope.Config
	profiler := &fakeProfiler{}
	withFakePyroscopeStart(t, func(cfg pyroscope.Config) (profilerHandle, error) {
		captured = cfg
		return profiler, nil
	})

	opts := profilingOpts(srv.URL, WithProfilingAuthHeaders(headers))

	sdk, err := Init(context.Background(), opts...)
	require.NoError(t, err)
	headers["Authorization"] = "Bearer mutated"
	require.NoError(t, sdk.Shutdown(context.Background()))

	assert.True(t, profiler.stopped, "SDK shutdown should stop the profiler")
	assert.Equal(t, "profiled-svc", captured.ApplicationName)
	assert.Equal(t, "http://alloy.infra.svc.cluster.local:4040", captured.ServerAddress)
	assert.Equal(t, "Bearer original", captured.HTTPHeaders["Authorization"])
	assert.Equal(t, "tenant-a", captured.HTTPHeaders["X-Scope-OrgID"])
	assert.Equal(t, "profiled-svc", captured.Tags["service_name"])
	assert.Equal(t, "platform", captured.Tags["service_namespace"])
	assert.Equal(t, "1.2.3", captured.Tags["service_version"])
	assert.Equal(t, "production", captured.Tags["service_env"])
	assert.ElementsMatch(t, []pyroscope.ProfileType{
		pyroscope.ProfileCPU,
		pyroscope.ProfileAllocObjects,
		pyroscope.ProfileAllocSpace,
		pyroscope.ProfileInuseObjects,
		pyroscope.ProfileInuseSpace,
	}, captured.ProfileTypes)
}

func TestInit_ProfilingStartFailureLogsAndContinues(t *testing.T) {
	srv := newFakeOTLPServer(t)
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		return nil, errors.New("start failed")
	})

	sdk, err := Init(context.Background(), profilingOpts(srv.URL)...)
	require.NoError(t, err)
	defer func() { require.NoError(t, sdk.Shutdown(context.Background())) }()

	assert.Contains(t, reflect.TypeOf(sdk.TracerProvider()).String(), "otelpyroscope",
		"the trace-to-profile wrapper should stay installed even if profiler start fails")
}

func TestInit_ProfilingRejectsSecondActiveProfiler(t *testing.T) {
	srv := newFakeOTLPServer(t)
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		return &fakeProfiler{}, nil
	})

	first, err := Init(context.Background(), profilingOpts(srv.URL)...)
	require.NoError(t, err)

	second, err := Init(context.Background(), profilingOpts(srv.URL)...)
	require.Error(t, err)
	assert.Nil(t, second)
	assert.ErrorIs(t, err, errProfilerAlreadyStarted)

	require.NoError(t, first.Shutdown(context.Background()))
}
