package metrics

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// resourceConstantLabelKeys are the resource attributes the Prometheus path
// promotes to a constant label on every exported series
// (initPrometheus → otelprom.WithResourceAsConstantLabels). They come from
// the pinned semconv package so a semconv upgrade that renames one surfaces
// here through the compiler rather than leaving a stale literal behind.
var resourceConstantLabelKeys = []attribute.Key{
	semconv.ServiceNamespaceKey,
	semconv.ServiceNameKey,
	semconv.ServiceVersionKey,
	semconv.DeploymentEnvironmentNameKey,
}

// exposedFormatLabels are label names the Prometheus exposition format itself
// synthesizes: `le` on every histogram bucket and `quantile` on summaries.
// otelprom never emits summaries, but reserving both costs nothing. A
// datapoint attribute rendering as `le` would not fail gathering — the
// duplicate only appears in the rendered text (`_bucket{le="x",le="1"}`), so
// ContinueOnError cannot catch it — and Prometheus then rejects the whole
// scrape, healthy families included.
var exposedFormatLabels = []string{"le", "quantile"}

// scopeLabelPrefix is the prefix otelprom puts on every label it derives from
// the instrumentation scope: otel_scope_name / _version / _schema_url on
// every series, plus otel_scope_<attr> for each instrumentation-scope
// attribute a meter was created with. The prefix is reserved as a whole
// rather than enumerated.
const scopeLabelPrefix = "otel_scope_"

// reservedPromLabels holds, in their post-normalization Prometheus form, the
// exact label names the exporter or the exposition format already owns on a
// series: the resource constants above and exposedFormatLabels. It is built
// once from those sources so the two never drift, and grouped by first byte
// so IsReservedAttributeKey can reject most keys after one comparison.
//
// A datapoint attribute that normalizes to one of these names cannot be
// exported: otelprom appends the constant labels after the datapoint's own
// with no de-duplication, client_golang rejects the resulting family
// ("duplicate label names in constant and variable labels"), and because
// aggregation is cumulative the bad series lives until the process restarts.
// With promhttp's default error handling that turned one mislabeled Record
// call into a permanent HTTP 500 on /metrics for the whole pod.
var reservedPromLabels = buildReservedPromLabels()

func buildReservedPromLabels() map[byte][]string {
	out := make(map[byte][]string)
	add := func(label string) {
		if label == "" {
			return
		}
		out[label[0]] = append(out[label[0]], label)
	}
	for _, k := range resourceConstantLabelKeys {
		add(NormalizePrometheusLabelName(string(k)))
	}
	for _, l := range exposedFormatLabels {
		add(l)
	}
	return out
}

// IsReservedAttributeKey reports whether key would render as a Prometheus
// label the exporter or the exposition format already owns (see
// reservedPromLabels and scopeLabelPrefix). The check runs on the normalized
// form, so "service.name", "service_name" and "service-name" are all
// reserved.
//
// It is installed as an AttributeFilter on every stream, which the OTel SDK
// evaluates for every attribute of every measurement, so it must not
// allocate: the comparison walks the key applying the normalization rules on
// the fly instead of materializing the normalized string.
func IsReservedAttributeKey(key attribute.Key) bool {
	k := string(key)
	if k == "" {
		return false
	}
	// Two shapes the translator never turns into an exportable label:
	//   - "__x__": the translator keeps the surrounding double underscores
	//     (Prometheus' reserved-name convention) and client_golang then
	//     rejects the label as internal;
	//   - a key with no alphanumeric rune at all normalizes to underscores
	//     only, which the translator refuses outright.
	// Either fails the whole family at gather time, so both are reserved.
	if isTranslatorReservedShape(k) || !hasAlphanumeric(k) {
		return true
	}
	// A key whose normalized form starts with a digit is prefixed "key_",
	// which no reserved label starts with; any other first byte survives
	// normalization as itself or as '_', so it selects the candidate group.
	if k[0] == scopeLabelPrefix[0] && normalizedHasPrefix(k, scopeLabelPrefix) {
		return true
	}
	for _, want := range reservedPromLabels[k[0]] {
		if normalizedEquals(k, want) {
			return true
		}
	}
	return false
}

// isTranslatorReservedShape mirrors otlptranslator's isReservedLabel: a key of
// at least four bytes that both starts and ends with "__" is treated as a
// Prometheus reserved name and exported with those underscores intact.
func isTranslatorReservedShape(k string) bool {
	return len(k) >= 4 && k[0] == '_' && k[1] == '_' && k[len(k)-1] == '_' && k[len(k)-2] == '_'
}

// hasAlphanumeric reports whether k contains at least one rune that survives
// label normalization unchanged. Without one the normalized label would be
// underscores only, which the translator rejects.
func hasAlphanumeric(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

// normalizedEquals reports whether NormalizePrometheusLabelName(key) == want
// without allocating. want must already be in normalized form and must not
// start with a digit, and key must not be of the "__x__" reserved shape;
// IsReservedAttributeKey settles both cases before calling it.
func normalizedEquals(key, want string) bool {
	n, complete := normalizedMatchLen(key, want)
	return complete && n == len(want)
}

// normalizedHasPrefix reports whether NormalizePrometheusLabelName(key) starts
// with prefix, under the same conditions as normalizedEquals.
func normalizedHasPrefix(key, prefix string) bool {
	n, _ := normalizedMatchLen(key, prefix)
	return n == len(prefix)
}

// normalizedMatchLen walks key applying the Prometheus label normalization
// rules on the fly and compares the result byte by byte against want. It
// returns how many bytes of want were matched and whether key was consumed
// entirely while matching (false as soon as a byte differs or key outgrows
// want). It never allocates.
func normalizedMatchLen(key, want string) (matched int, complete bool) {
	i := 0 // next byte of want to match
	prevUnderscore := false
	for _, r := range key {
		var out byte
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			out = byte(r)
			prevUnderscore = false
		case prevUnderscore:
			continue // collapsed into the previous '_'
		default:
			out = '_'
			prevUnderscore = true
		}
		if i >= len(want) || want[i] != out {
			return i, false
		}
		i++
	}
	return i, true
}

// notReserved is the attribute.Filter that keeps every attribute except the
// reserved ones.
func notReserved(kv attribute.KeyValue) bool {
	return !IsReservedAttributeKey(kv.Key)
}

// NormalizePrometheusLabelName mirrors the classic non-UTF8 Prometheus label
// normalization that otelprom (via github.com/prometheus/otlptranslator)
// applies on export: any rune outside [a-zA-Z0-9] becomes '_', adjacent
// underscores collapse to a single one, a leading digit is prefixed with
// "key_", and a key shaped "__x__" keeps its surrounding double underscores
// (the translator's reserved-name rule) so the result is a syntactically
// valid Prometheus label.
//
// Reproducing the same rules inside the SDK is what lets it detect collisions
// before they reach the exporter, where two distinct attribute keys would
// otherwise be silently merged into one label, or fail the scrape outright.
func NormalizePrometheusLabelName(key string) string {
	if key == "" {
		return ""
	}
	if isTranslatorReservedShape(key) {
		return "__" + NormalizePrometheusLabelName(key[2:len(key)-2]) + "__"
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
