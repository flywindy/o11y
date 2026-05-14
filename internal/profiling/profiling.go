// Package profiling encapsulates the SDK's Pyroscope profiler lifecycle.
package profiling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
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
// subsystem.
type Config struct {
	ServiceName string
	Endpoint    string
	AuthHeaders map[string]string
	Resource    *resource.Resource
	Logger      *slog.Logger
}

// Start starts the Pyroscope profiler and returns a shutdown function.
func Start(cfg Config) (func(context.Context) error, error) {
	profilerMu.Lock()
	defer profilerMu.Unlock()

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
		Logger: pyroscopeSlogAdapter{logger: cfg.Logger},
	})
	if err != nil {
		return nil, err
	}
	profilerStarted = true

	return func(context.Context) error {
		err := profiler.Stop()
		if err == nil {
			profilerMu.Lock()
			profilerStarted = false
			profilerMu.Unlock()
		}
		return err
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

type pyroscopeSlogAdapter struct {
	logger *slog.Logger
}

func (a pyroscopeSlogAdapter) Infof(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.Info(fmt.Sprintf(format, args...))
	}
}

func (a pyroscopeSlogAdapter) Debugf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.Debug(fmt.Sprintf(format, args...))
	}
}

func (a pyroscopeSlogAdapter) Errorf(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.Error(fmt.Sprintf(format, args...))
	}
}
