package profiling

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/grafana/pyroscope-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
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

func testResource(t *testing.T) *resource.Resource {
	t.Helper()
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("profiled-svc"),
			semconv.ServiceNamespaceKey.String("platform"),
			semconv.ServiceVersionKey.String("1.2.3"),
			semconv.DeploymentEnvironmentNameKey.String("production"),
		),
	)
	require.NoError(t, err)
	return res
}

func TestStart_ConfiguresPyroscopeWithResourceTagsAndHeaders(t *testing.T) {
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

	closer, err := Start(Config{
		ServiceName: "profiled-svc",
		Endpoint:    "http://alloy.infra.svc.cluster.local:4040",
		AuthHeaders: headers,
		Resource:    testResource(t),
		Logger:      slog.Default(),
	})
	require.NoError(t, err)
	headers["Authorization"] = "Bearer mutated"
	require.NoError(t, closer(context.Background()))

	assert.True(t, profiler.stopped, "shutdown should stop the profiler")
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

func TestStart_ReturnsErrorWithoutConsumingSingletonSlot(t *testing.T) {
	startErr := errors.New("start failed")
	var calls int
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		calls++
		if calls == 1 {
			return nil, startErr
		}
		return &fakeProfiler{}, nil
	})

	_, err := Start(Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.ErrorIs(t, err, startErr)

	closer, err := Start(Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)
	require.NoError(t, closer(context.Background()))
}

func TestStart_RejectsSecondActiveProfiler(t *testing.T) {
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		return &fakeProfiler{}, nil
	})

	first, err := Start(Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	second, err := Start(Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.ErrorIs(t, err, ErrAlreadyStarted)
	assert.Nil(t, second)

	require.NoError(t, first(context.Background()))
}
