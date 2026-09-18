// Package profiling encapsulates the SDK's Pyroscope profiler lifecycle.
package profiling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	"github.com/flywindy/o11y/internal/redact"
)

const maxPyroscopeTagValueBytes = 1024

// ErrAlreadyStarted is returned when profiling is already active in the
// current process. Go's pprof profiler is process-wide, so the SDK allows only
// one successful Pyroscope profiler session at a time.
var ErrAlreadyStarted = errors.New("profiling is already active in this process")

var (
	profilerMu      sync.Mutex
	profilerStarted bool
	tagWarnOnce     sync.Once
)

type profilerHandle interface {
	Stop() error
}

var pyroscopeStart = func(cfg pyroscope.Config) (profilerHandle, error) {
	return pyroscope.Start(cfg)
}

// Config is the subset of SDK configuration needed by the profiling
// subsystem. It specifies the Pyroscope endpoint, authentication headers,
// resource-derived tags, and logger used by the profiler integration.
type Config struct {
	ServiceName string
	Endpoint    string
	AuthHeaders map[string]string
	Resource    *resource.Resource
	Logger      *slog.Logger
}

// Start starts the Pyroscope profiler and returns a shutdown function. The
// shutdown function honours its context: Pyroscope's Stop flushes the
// uploader, which can wait up to its own 30s client timeout, so when the
// context ends first the function returns ctx.Err() while Stop completes in
// the background and releases the profiler slot when it does.
func Start(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	profilerMu.Lock()
	defer profilerMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if profilerStarted {
		return nil, ErrAlreadyStarted
	}

	profiler, err := pyroscopeStart(pyroscope.Config{
		ApplicationName: cfg.ServiceName,
		ServerAddress:   cfg.Endpoint,
		HTTPHeaders:     cloneStringMap(cfg.AuthHeaders),
		Tags:            profileTagsFromResource(cfg.Resource, cfg.Logger),
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
		},
		Logger: newPyroscopeSlogAdapter(cfg),
	})
	if err != nil {
		return nil, err
	}
	profilerStarted = true

	return func(ctx context.Context) error {
		// Stop flushes the uploader, which waits on requests it makes with
		// its own 30s client timeout and ignores any context, so it runs on
		// a goroutine and the closer returns ctx.Err() if the caller's
		// deadline ends first: SDK.Shutdown shares its deadline across the
		// pillars and the profiler stops first, so a stalled upload must not
		// consume the tracer's and logger's share. Stop then finishes on its
		// own and releases the process-wide pprof slot when it does.
		//
		// The slot is released whatever Stop reports. The flag tracks "this
		// process holds the profiler", not "shutdown was clean": the profiler
		// is no longer running either way, and SDK.Shutdown runs each closer
		// at most once, so a flag left set on a Stop error could never be
		// cleared and every later Start would fail with ErrAlreadyStarted
		// for the life of the process.
		done := make(chan error, 1)
		go func() {
			err := profiler.Stop()
			profilerMu.Lock()
			profilerStarted = false
			profilerMu.Unlock()
			done <- err
		}()
		// A context that is done wins over a Stop that has finished, whether
		// it was done before the closer ran or ended while Stop was running:
		// select picks at random between two ready cases, and the contract
		// is that an expired context is reported as such.
		select {
		case err := <-done:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func profileTagsFromResource(res *resource.Resource, logger *slog.Logger) map[string]string {
	if res == nil {
		return nil
	}

	tags := make(map[string]string)
	attrSet := res.Set()
	for _, mapping := range []struct {
		attr attribute.Key
		tag  string
	}{
		{semconv.ServiceNameKey, "service_name"},
		{semconv.ServiceNamespaceKey, "service_namespace"},
		{semconv.ServiceVersionKey, "service_version"},
		{semconv.DeploymentEnvironmentNameKey, "service_env"},
		{semconv.HostNameKey, "hostname"},
		{semconv.K8SPodNameKey, "pod"},
		{semconv.K8SNamespaceNameKey, "k8s_namespace"},
	} {
		value, ok := attrSet.Value(mapping.attr)
		if !ok || value.Type() != attribute.STRING {
			continue
		}
		tagValue := strings.TrimSpace(value.AsString())
		if tagValue == "" {
			continue
		}
		tags[mapping.tag] = truncatePyroscopeTagValue(mapping.tag, tagValue, logger)
	}
	return tags
}

func truncatePyroscopeTagValue(tag, value string, logger *slog.Logger) string {
	if len(value) <= maxPyroscopeTagValueBytes {
		return value
	}
	if logger != nil {
		tagWarnOnce.Do(func() {
			logger.Warn("Pyroscope tag value truncated",
				slog.String("tag", tag),
				slog.Int("max_bytes", maxPyroscopeTagValueBytes),
			)
		})
	}
	lastBoundary := 0
	for i := range value {
		if i > maxPyroscopeTagValueBytes {
			return value[:lastBoundary]
		}
		lastBoundary = i
	}
	return value[:lastBoundary]
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// pyroscopeSlogAdapter forwards pyroscope-go's logger calls to the SDK's
// logger with the profiling endpoint's credentials, and the configured auth
// header values, removed from every line.
//
// pyroscope formats the ingest URL into its own messages and redacts nothing:
// `uploading at %s` on every upload (upstream/remote/remote.go:193, Debugf),
// and the *url.Error behind a failed upload (:272, Errorf). The endpoint may
// carry userinfo, because the SDK keeps honouring
// http://user:pass@host profiling endpoints — see redact.URL — and o11y.go
// redacts that same endpoint everywhere else it is logged. So the Debugf line
// prints the password in full, and the Errorf one prints the username, since
// net/http masks only the password when it builds the *url.Error.
//
// The scrub therefore runs on all three methods rather than the ones that look
// risky today: the adapter has no say in what the upstream chooses to format
// into a message, and a version bump could move the address onto any of them.
type pyroscopeSlogAdapter struct {
	logger   *slog.Logger
	endpoint string
	secrets  []string
}

// newPyroscopeSlogAdapter builds the adapter pyroscope logs through, carrying
// the values that must never reach a record.
func newPyroscopeSlogAdapter(cfg Config) pyroscopeSlogAdapter {
	return pyroscopeSlogAdapter{
		logger:   cfg.Logger,
		endpoint: cfg.Endpoint,
		secrets:  authHeaderSecrets(cfg.AuthHeaders),
	}
}

// authHeaderSecrets returns the configured profiling auth header values, so
// redact.Secrets can take them out of a line that echoes one. The values are
// sorted to keep the result independent of map iteration order.
func authHeaderSecrets(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	secrets := make([]string, 0, len(headers))
	for _, value := range headers {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	sort.Strings(secrets)
	return secrets
}

// scrub renders one pyroscope log line with the endpoint's credentials and the
// configured auth header values removed.
func (a pyroscopeSlogAdapter) scrub(format string, args ...any) string {
	return redact.Secrets(redact.InText(fmt.Sprintf(format, args...), a.endpoint), a.secrets...)
}

// logContext is the context these records carry. pyroscope calls the adapter
// from its own collector and upload goroutines, which have no request context
// of their own; Start's context is not reused because it would attach that one
// call's span to every upload line for the life of the profiler, and is
// usually cancelled long before they are written.
func logContext() context.Context { return context.Background() }

func (a pyroscopeSlogAdapter) Infof(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.InfoContext(logContext(), a.scrub(format, args...))
	}
}

func (a pyroscopeSlogAdapter) Debugf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.DebugContext(logContext(), a.scrub(format, args...))
	}
}

func (a pyroscopeSlogAdapter) Errorf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.ErrorContext(logContext(), a.scrub(format, args...))
	}
}
