package metrics

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// resourceConstantLabelKeys are the resource attributes the Prometheus path
// promotes to a constant label on every exported series
// (initPrometheus → otelprom.WithResourceAsConstantLabels).
var resourceConstantLabelKeys = []string{
	"service.namespace",
	"service.name",
	"service.version",
	"deployment.environment.name",
}

// reservedPromLabels lists, in their post-normalization Prometheus form, the
// label names the exporter already owns on every series: the four resource
// constants above and the three scope labels otelprom adds
// (otel_scope_name / _version / _schema_url).
//
// A datapoint attribute that normalizes to one of these names cannot be
// exported: otelprom appends the constant labels after the datapoint's own
// with no de-duplication, client_golang rejects the resulting family
// ("duplicate label names in constant and variable labels"), and because
// aggregation is cumulative the bad series lives until the process restarts.
// With promhttp's default error handling that turned one mislabeled Record
// call into a permanent HTTP 500 on /metrics for the whole pod.
var reservedPromLabels = map[string]struct{}{
	"service_namespace":           {},
	"service_name":                {},
	"service_version":             {},
	"deployment_environment_name": {},
	"otel_scope_name":             {},
	"otel_scope_version":          {},
	"otel_scope_schema_url":       {},
}

// IsReservedAttributeKey reports whether key would render as a Prometheus
// label the exporter already owns (see reservedPromLabels). The check runs
// on the normalized form, so "service.name", "service_name" and
// "service-name" are all reserved.
func IsReservedAttributeKey(key attribute.Key) bool {
	_, ok := reservedPromLabels[NormalizePrometheusLabelName(string(key))]
	return ok
}

// notReserved is the attribute.Filter that keeps every attribute except the
// reserved ones.
func notReserved(kv attribute.KeyValue) bool {
	return !IsReservedAttributeKey(kv.Key)
}

// NormalizePrometheusLabelName mirrors the classic non-UTF8 Prometheus label
// normalization that otelprom (via github.com/prometheus/otlptranslator)
// applies on export: any rune outside [a-zA-Z0-9] becomes '_', adjacent
// underscores collapse to a single one, and a leading digit is prefixed with
// "key_" so the result is a syntactically valid Prometheus label.
//
// Reproducing the same rules inside the SDK is what lets it detect collisions
// before they reach the exporter, where two distinct attribute keys would
// otherwise be silently merged into one label, or fail the scrape outright.
func NormalizePrometheusLabelName(key string) string {
	if key == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(key))
	prevUnderscore := false
	for _, r := range key {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteRune('_')
			prevUnderscore = true
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if first := out[0]; first >= '0' && first <= '9' {
		out = "key_" + out
	}
	return out
}

// guardReservedKeys returns the views the MeterProvider should be built with
// so that no stream, on any instrument, carries a reserved attribute key.
//
// Two mechanisms, because the OTel SDK applies every matching view and only
// falls back to its implicit default stream when none matched:
//
//   - each configured view is wrapped so the stream it returns additionally
//     filters out reserved keys (composed with the view's own filter, if any);
//   - a catch-all view is appended for instruments no configured view matches.
//     It reproduces the SDK's implicit default stream (same name, description
//     and unit, reader-default aggregation) plus the reserved-key filter, and
//     it deliberately declines instruments a configured view already matched —
//     a second stream on those would carry a different aggregation and export
//     as a duplicate family.
//
// The catch-all also uses the SDK's exemplar selector that drops filtered
// attributes, so a dropped reserved key does not resurface as an exemplar
// label. Nothing else about the stream changes, so instruments without a
// reserved key are exported exactly as before.
func guardReservedKeys(configured []sdkmetric.View) []sdkmetric.View {
	out := make([]sdkmetric.View, 0, len(configured)+1)
	for _, v := range configured {
		out = append(out, withReservedKeyFilter(v))
	}
	out = append(out, catchAllReservedKeyFilter(configured))
	return out
}

func withReservedKeyFilter(v sdkmetric.View) sdkmetric.View {
	return func(inst sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		stream, ok := v(inst)
		if !ok {
			return stream, false
		}
		stream.AttributeFilter = composeFilters(stream.AttributeFilter, notReserved)
		return stream, true
	}
}

func catchAllReservedKeyFilter(configured []sdkmetric.View) sdkmetric.View {
	return func(inst sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		for _, v := range configured {
			if _, ok := v(inst); ok {
				return sdkmetric.Stream{}, false
			}
		}
		return sdkmetric.Stream{
			Name:                              inst.Name,
			Description:                       inst.Description,
			Unit:                              inst.Unit,
			AttributeFilter:                   notReserved,
			ExemplarReservoirProviderSelector: dropFilteredAttrsExemplarSelector,
		}, true
	}
}

// composeFilters returns a filter that keeps an attribute only when both
// filters keep it. A nil inner filter keeps everything.
func composeFilters(inner, outer attribute.Filter) attribute.Filter {
	if inner == nil {
		return outer
	}
	return func(kv attribute.KeyValue) bool {
		return inner(kv) && outer(kv)
	}
}
