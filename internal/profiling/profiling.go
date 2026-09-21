// Package profiling encapsulates the SDK's Pyroscope profiler lifecycle.
package profiling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
		secrets:  authHeaderSecrets(cfg.Endpoint, cfg.AuthHeaders),
	}
}

// authHeaderSecrets returns the configured profiling auth headers, so
// redact.Secrets can take them out of a line that echoes one.
//
// Names as well as values. A credential pasted into the wrong side of a header
// configuration is still a credential, diagnosticSecrets already treats OTLP
// header names that way, and the pinned uploader puts a failed upload's whole
// response body into its ERROR line — so a server that reports the headers it
// received can name one. The cost is that a line legitimately mentioning a
// configured name loses it: with "Authorization" configured, pyroscope's
// auth-token deprecation warning reads "set the [redacted] header manually".
// That is the trade this package makes everywhere else.
//
// Each of those is listed in four renderings, matching diagnosticSecrets:
//
//   - as configured, because something may echo the configuration;
//   - as net/http puts it on the wire, which for a value is trimmed
//     (redact.HeaderWireValue) and for a name is the canonical MIME form
//     (redact.HeaderWireName), because that is what a server receives;
//   - and each of those as %q and as a JSON document render it
//     (redact.Renderings), because redact.Secrets matches literally and a
//     caller that quotes a value, or a Go server that puts it in a JSON error
//     body, produces text the raw form does not cover.
//
// Nothing in the pinned pyroscope or net/http is known to quote a header value
// — Go's own invalid-header error names the header, not what it held — but
// this adapter's whole premise is that it does not get to choose what the
// upstream formats into a message.
//
// The result is sorted to keep it independent of map iteration order.
func authHeaderSecrets(endpoint string, headers map[string]string) []string {
	seen := make(map[string]struct{}, len(headers)*8)
	secrets := make([]string, 0, len(headers)*8)
	add := func(forms []string) {
		for _, form := range forms {
			if form == "" {
				continue
			}
			if _, dup := seen[form]; dup {
				continue
			}
			seen[form] = struct{}{}
			secrets = append(secrets, form)
		}
	}
	for name, value := range headers {
		add(redact.HeaderNameForms(name))
		add(redact.HeaderValueForms(value))
	}
	// The endpoint's own userinfo is a credential the SDK never configured as a
	// header and yet sends as one: http.Client derives
	// "Authorization: Basic base64(user:pass)" from it. The base64 contains
	// neither the username nor the password as a substring, so nothing else in
	// this list would match a server that echoed the header back.
	//
	// Both endpoints are covered, because the one the SDK was given is not
	// necessarily the one pyroscope uploads to: Start overrides ServerAddress
	// from PYROSCOPE_ADHOC_SERVER_ADDRESS before it builds the uploader
	// (pyroscope-go api.go:57), and an override carrying userinfo would
	// otherwise put a credential on the wire that this list had never seen.
	for _, addr := range []string{endpoint, adhocServerAddress()} {
		basic := redact.BasicAuthHeader(addr)
		if basic == "" {
			continue
		}
		add(redact.HeaderValueForms(basic))
		// Also without the scheme, for a report that names only the token.
		add(redact.HeaderValueForms(strings.TrimPrefix(basic, "Basic ")))
	}
	if len(secrets) == 0 {
		return nil
	}
	sort.Strings(secrets)
	return secrets
}

// adhocServerAddressEnv is the variable pyroscope.Start reads to override the
// configured ingest address, before it builds the uploader.
const adhocServerAddressEnv = "PYROSCOPE_ADHOC_SERVER_ADDRESS"

// adhocServerAddress returns the ingest address pyroscope will actually use
// when that override is set, or "" when it is not.
//
// Reading it is not the same as honouring it: the SDK neither sets nor clears
// the variable, and Start's own override still decides the address. This only
// lets the redaction know which address the credential it may have to scrub
// came from.
func adhocServerAddress() string {
	return os.Getenv(adhocServerAddressEnv)
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

// Infof records pyroscope's informational lines, which include its startup
// banner and the auth-token deprecation warning, at INFO.
func (a pyroscopeSlogAdapter) Infof(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.InfoContext(logContext(), a.scrub(format, args...))
	}
}

// Debugf records pyroscope's per-upload and per-collector lines at DEBUG.
// These are the noisy ones — one per upload interval — and the ones that
// carry the ingest URL.
func (a pyroscopeSlogAdapter) Debugf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.DebugContext(logContext(), a.scrub(format, args...))
	}
}

// Errorf records a failed upload or a recovered panic from pyroscope's own
// goroutines at ERROR. The message may hold the *url.Error behind a failed
// upload, which is why the scrub runs here and not only on Debugf.
func (a pyroscopeSlogAdapter) Errorf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.ErrorContext(logContext(), a.scrub(format, args...))
	}
}
