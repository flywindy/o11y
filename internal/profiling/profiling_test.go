package profiling

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

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

	closer, err := Start(context.Background(), Config{
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

	_, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.ErrorIs(t, err, startErr)

	closer, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)
	require.NoError(t, closer(context.Background()))
}

func TestStart_RejectsSecondActiveProfiler(t *testing.T) {
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		return &fakeProfiler{}, nil
	})

	first, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	second, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.ErrorIs(t, err, ErrAlreadyStarted)
	assert.Nil(t, second)

	require.NoError(t, first(context.Background()))
}

func TestStart_ReturnsCanceledContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	closer, err := Start(ctx, Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, closer)
}

func TestTruncatePyroscopeTagValue_PreservesUTF8(t *testing.T) {
	value := strings.Repeat("a", maxPyroscopeTagValueBytes-1) + "\u754c"

	truncated := truncatePyroscopeTagValue("service_name", value, slog.Default())

	assert.True(t, utf8.ValidString(truncated))
	assert.Equal(t, maxPyroscopeTagValueBytes-1, len(truncated))
	assert.Equal(t, strings.Repeat("a", maxPyroscopeTagValueBytes-1), truncated)
}

func TestPyroscopeSlogAdapter_InfofUsesInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := pyroscopeSlogAdapter{logger: logger}

	adapter.Infof("upload %s", "started")

	output := buf.String()
	assert.Contains(t, output, `"level":"INFO"`)
	assert.Contains(t, output, `"msg":"upload started"`)
}

// TestCloser_ReleasesSlotEvenWhenStopFails pins that a failed Stop does not
// strand the process-wide pprof slot. The closer used to clear profilerStarted
// only on a nil error, and SDK.Shutdown runs each closer at most once, so a
// single Stop failure made every later Start in the process return
// ErrAlreadyStarted with no way to recover.
func TestCloser_ReleasesSlotEvenWhenStopFails(t *testing.T) {
	stopErr := errors.New("pyroscope: flush failed")
	failing := &fakeProfiler{stopErr: stopErr}
	healthy := &fakeProfiler{}
	// The first Start gets a profiler whose Stop fails; a later one gets a
	// working profiler, so the only thing that can block it is a stranded slot.
	starts := 0
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) {
		starts++
		if starts == 1 {
			return failing, nil
		}
		return healthy, nil
	})

	closer, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	// The error still reaches the caller — the slot is released regardless.
	require.ErrorIs(t, closer(context.Background()), stopErr)
	assert.True(t, failing.stopped)

	second, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err, "a failed Stop must not permanently poison profiling for the process")
	require.NoError(t, second(context.Background()))
}
