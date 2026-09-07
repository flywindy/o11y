package metrics

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
)

// GuardScopeAttributes wraps mp so that instrumentation-scope attributes a
// meter is created with cannot collide with the labels otelprom derives from
// the scope itself.
//
// otelprom renders every scope attribute as otel_scope_<normalized key>
// beside its fixed otel_scope_name / otel_scope_version /
// otel_scope_schema_url labels, with no de-duplication. A scope attribute
// named "name", or two scope attributes that normalize to the same label
// ("app.tier" and "app_tier"), therefore fail every family of that meter at
// gather time — and, unlike datapoint attributes, scope attributes never pass
// through a stream's AttributeFilter, so the reserved-key views cannot help.
// The wrapper drops such attributes at Meter creation, keeps the rest, and
// logs each drop once at WARN when logger is non-nil. Meters created without
// scope attributes pass straight through.
func GuardScopeAttributes(mp metric.MeterProvider, logger *slog.Logger) metric.MeterProvider {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &scopeGuardedMeterProvider{inner: mp, logger: logger}
}

type scopeGuardedMeterProvider struct {
	embedded.MeterProvider

	inner  metric.MeterProvider
	logger *slog.Logger
}

// Meter implements metric.MeterProvider.
func (p *scopeGuardedMeterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	cfg := metric.NewMeterConfig(opts...)
	attrs := cfg.InstrumentationAttributes()
	if attrs.Len() == 0 {
		return p.inner.Meter(name, opts...)
	}
	kept, dropped := sanitizeScopeAttributes(attrs)
	if len(dropped) == 0 {
		return p.inner.Meter(name, opts...)
	}
	for _, kv := range dropped {
		p.logger.WarnContext(context.Background(),
			"instrumentation-scope attribute dropped: its Prometheus label would collide with one the exporter already emits",
			slog.String("meter", name),
			slog.String("attribute", string(kv.Key)),
			slog.String("label", scopeLabelPrefix+NormalizePrometheusLabelName(string(kv.Key))),
		)
	}
	return p.inner.Meter(name,
		metric.WithInstrumentationVersion(cfg.InstrumentationVersion()),
		metric.WithSchemaURL(cfg.SchemaURL()),
		metric.WithInstrumentationAttributes(kept...),
	)
}

// scopeBuiltinLabels are the scope-derived labels otelprom always emits, in
// the form a scope attribute key would have to normalize to in order to
// collide with them (the otel_scope_ prefix is added by the exporter).
var scopeBuiltinLabels = map[string]struct{}{
	"name":       {},
	"version":    {},
	"schema_url": {},
}

// sanitizeScopeAttributes splits attrs into the ones safe to export and the
// ones that would collide: keys normalizing to a built-in scope label, keys
// normalizing to no usable label (empty, or a bare "_" from a key with no
// alphanumerics), and keys normalizing to the same label as an earlier kept
// attribute.
func sanitizeScopeAttributes(attrs attribute.Set) (kept, dropped []attribute.KeyValue) {
	seen := make(map[string]struct{}, attrs.Len())
	for _, kv := range attrs.ToSlice() {
		label := NormalizePrometheusLabelName(string(kv.Key))
		if label == "" || label == "_" {
			dropped = append(dropped, kv)
			continue
		}
		if _, builtin := scopeBuiltinLabels[label]; builtin {
			dropped = append(dropped, kv)
			continue
		}
		if _, dup := seen[label]; dup {
			dropped = append(dropped, kv)
			continue
		}
		seen[label] = struct{}{}
		kept = append(kept, kv)
	}
	return kept, dropped
}
