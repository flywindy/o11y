package profiling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grafana/pyroscope-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

type fakeProfiler struct {
	stopErr   error
	stopped   bool
	stopCalls int
	// block, when set, holds Stop until it is closed, standing in for a
	// Pyroscope uploader waiting on a stalled request.
	block chan struct{}
	// onStop, when set, runs inside Stop before it returns, so a test can
	// end the closer's context while Stop is still in flight.
	onStop func()
}

func (f *fakeProfiler) Stop() error {
	if f.block != nil {
		<-f.block
	}
	if f.onStop != nil {
		f.onStop()
	}
	f.stopped = true
	f.stopCalls++
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

// TestPyroscopeSlogAdapter_RedactsTheEndpointAndAuthHeaders pins that no
// pyroscope log line can carry the profiling endpoint's credentials or a
// configured auth header value.
//
// The messages are the ones pyroscope-go v1.3.0 actually produces. `uploading
// at %s` is upstream/remote/remote.go:193 (Debugf) with the parsed ingest URL,
// userinfo intact. `upload profile: %v` is :272 (Errorf) with the *url.Error
// net/http returns, which masks the password and keeps the username — so the
// error path leaks at ERROR level, not only at DEBUG.
func TestPyroscopeSlogAdapter_RedactsTheEndpointAndAuthHeaders(t *testing.T) {
	const (
		endpoint = "http://alice:s3cretpw@pyroscope:4040"
		token    = "Bearer glc_eyJvIjoiMTIzNDU2In0="
	)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := newPyroscopeSlogAdapter(Config{
		Logger:      logger,
		Endpoint:    endpoint,
		AuthHeaders: map[string]string{"Authorization": token},
	})

	adapter.Debugf("uploading at %s", endpoint+"/ingest?name=svc&spyName=gospy")
	adapter.Errorf("upload profile: %v",
		fmt.Errorf(`Post "http://alice:***@pyroscope:4040/ingest?name=svc": dial tcp: i/o timeout`)) //nolint:err113
	adapter.Infof("sending Authorization header %s", token)

	output := buf.String()
	assert.NotContains(t, output, "s3cretpw", "the endpoint password must never reach a record")
	assert.NotContains(t, output, "alice", "the endpoint username must never reach a record either")
	assert.NotContains(t, output, "glc_eyJvIjoiMTIzNDU2In0=", "a configured auth header value is a secret")
	assert.Contains(t, output, "pyroscope:4040", "the host stays, so the line still says where it was uploading")
	assert.Contains(t, output, "name=svc", "and so does the rest of the URL")
}

// TestPyroscopeSlogAdapter_LevelsAndEmptyLoggerAreUnchanged covers the parts of
// the adapter the redaction must not disturb: each method keeps its level, an
// endpoint with no credentials is echoed as the operator wrote it, and a nil
// logger is still a no-op rather than a panic.
func TestPyroscopeSlogAdapter_LevelsAndEmptyLoggerAreUnchanged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := newPyroscopeSlogAdapter(Config{Logger: logger, Endpoint: "http://pyroscope:4040"})

	adapter.Infof("upload %s", "started")
	adapter.Debugf("uploading at %s", "http://pyroscope:4040/ingest")
	adapter.Errorf("upload profile: %s", "refused")

	output := buf.String()
	assert.Contains(t, output, `"level":"INFO"`)
	assert.Contains(t, output, `"msg":"upload started"`)
	assert.Contains(t, output, `"level":"DEBUG"`)
	assert.Contains(t, output, `"msg":"uploading at http://pyroscope:4040/ingest"`)
	assert.Contains(t, output, `"level":"ERROR"`)
	assert.Contains(t, output, `"msg":"upload profile: refused"`)

	empty := newPyroscopeSlogAdapter(Config{})
	assert.NotPanics(t, func() {
		empty.Infof("x")
		empty.Debugf("x")
		empty.Errorf("x")
	})
}

// TestAuthHeaderSecrets_SortsAndDropsEmpties pins that the secret list a
// pyroscope line is scrubbed against does not depend on map iteration order,
// and that an empty header value is left out — redact.Secrets ignores it, and
// carrying it would only obscure what the adapter is actually guarding.
func TestAuthHeaderSecrets_SortsAndDropsEmpties(t *testing.T) {
	assert.Nil(t, authHeaderSecrets(nil))
	assert.Nil(t, authHeaderSecrets(map[string]string{}))
	assert.Equal(t, []string{"aaa", "bbb"}, authHeaderSecrets(map[string]string{
		"X-Scope-OrgID": "bbb",
		"Authorization": "aaa",
		"X-Empty":       "",
	}))
}

// TestCloser_HonoursContextWhileStopBlocks pins that a Stop stalled on the
// uploader does not hold the closer past the caller's deadline: the closer
// returns ctx.Err(), Stop finishes on its own, and the profiler slot is
// released once it has.
func TestCloser_HonoursContextWhileStopBlocks(t *testing.T) {
	release := make(chan struct{})
	stalled := &fakeProfiler{block: release}
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) { return stalled, nil })

	closer, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, closer(ctx), context.DeadlineExceeded)

	_, err = Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.ErrorIs(t, err, ErrAlreadyStarted, "the slot stays held while Stop is still running")

	close(release)
	require.Eventually(t, func() bool {
		profilerMu.Lock()
		defer profilerMu.Unlock()
		return !profilerStarted
	}, time.Second, 5*time.Millisecond, "the slot is released once Stop completes")
	assert.Equal(t, 1, stalled.stopCalls)
}

// TestCloser_ReportsAnAlreadyCancelledContext pins that a context that is
// done before the closer runs is reported as ctx.Err() even when Stop
// returns at once, and that Stop still ran and released the slot.
func TestCloser_ReportsAnAlreadyCancelledContext(t *testing.T) {
	fast := &fakeProfiler{}
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) { return fast, nil })

	closer, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, closer(ctx), context.Canceled)
	require.Eventually(t, func() bool {
		profilerMu.Lock()
		defer profilerMu.Unlock()
		return !profilerStarted
	}, time.Second, 5*time.Millisecond, "Stop still runs and releases the slot")
	assert.Equal(t, 1, fast.stopCalls)
}

// TestCloser_ReportsAContextCancelledWhileStopRuns pins the race the
// select cannot order on its own: when the context ends while Stop is in
// flight and Stop then completes, both cases are ready, and the closer
// must still report ctx.Err() rather than Stop's result.
func TestCloser_ReportsAContextCancelledWhileStopRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	racing := &fakeProfiler{onStop: cancel}
	withFakePyroscopeStart(t, func(pyroscope.Config) (profilerHandle, error) { return racing, nil })

	closer, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err)

	require.ErrorIs(t, closer(ctx), context.Canceled)
	require.Eventually(t, func() bool {
		profilerMu.Lock()
		defer profilerMu.Unlock()
		return !profilerStarted
	}, time.Second, 5*time.Millisecond, "Stop completed and released the slot")
	assert.Equal(t, 1, racing.stopCalls)
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
	assert.Equal(t, 1, failing.stopCalls, "the failing profiler must be stopped exactly once")

	second, err := Start(context.Background(), Config{ServiceName: "profiled-svc", Endpoint: "http://alloy:4040"})
	require.NoError(t, err, "a failed Stop must not permanently poison profiling for the process")
	require.NoError(t, second(context.Background()))
	assert.Equal(t, 1, healthy.stopCalls, "the replacement profiler must be stopped exactly once")
	assert.Equal(t, 1, failing.stopCalls, "the failed closer must not be re-invoked")
}
