// Package metricscap rewrites high-cardinality metric attribute values at
// export boundaries.
package metricscap

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// OverflowValue is the label value used after a cap is exceeded.
const OverflowValue = "other"

// Rule caps one attribute key for one instrument name.
type Rule struct {
	InstrumentName string
	Key            attribute.Key
	Max            int

	// ScopeName restricts the rule to one instrumentation scope. Empty matches
	// any scope. Instrument names are not unique across scopes —
	// db.client.operation.duration is emitted by several integrations and by
	// caller-defined instruments — so an integration-specific cap must name its
	// scope, or it would silently rewrite unrelated streams and make them share
	// its budget.
	ScopeName string

	// BudgetKey shares one distinct-value budget across every rule carrying the
	// same non-empty value. Empty gives the rule its own budget. Use it when two
	// instruments must agree on which values overflow, so a value is never
	// capped on one and preserved on the other.
	BudgetKey string
}

// PrometheusRule caps one label for one Prometheus metric family.
type PrometheusRule struct {
	MetricName string
	LabelName  string
	Max        int

	// ScopeName is matched against the otel_scope_name label, which otelprom
	// attaches to every series. Empty matches any scope. See Rule.ScopeName.
	ScopeName string

	// BudgetKey shares a budget across rules. See Rule.BudgetKey.
	BudgetKey string
}

// scopeNameLabel is the label otelprom renders the instrumentation scope name
// into; it is the Prometheus-side equivalent of ScopeMetrics.Scope.Name.
const scopeNameLabel = "otel_scope_name"

// Limiter rewrites capped attributes in metricdata.ResourceMetrics before
// they are exported.
type Limiter struct {
	buckets sync.Map
	rules   []Rule
}

// NewLimiter returns a Limiter for rules. Rules with Max <= 0 are ignored.
func NewLimiter(rules ...Rule) *Limiter {
	usable := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.InstrumentName == "" || r.Key == "" || r.Max <= 0 {
			continue
		}
		usable = append(usable, r)
	}
	return &Limiter{rules: usable}
}

// Rewrite applies the limiter rules to all matching datapoints in rm.
func (l *Limiter) Rewrite(rm *metricdata.ResourceMetrics) {
	if l == nil || len(l.rules) == 0 || rm == nil {
		return
	}
	for si := range rm.ScopeMetrics {
		scope := rm.ScopeMetrics[si].Scope.Name
		for mi := range rm.ScopeMetrics[si].Metrics {
			m := &rm.ScopeMetrics[si].Metrics[mi]
			for _, rule := range l.rulesFor(scope, m.Name) {
				rewriteMetric(m, l.bucket(rule), rule.Key)
			}
		}
	}
}

func (l *Limiter) rulesFor(scope, name string) []Rule {
	out := make([]Rule, 0, len(l.rules))
	for _, rule := range l.rules {
		if rule.InstrumentName != name {
			continue
		}
		if rule.ScopeName != "" && rule.ScopeName != scope {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func (l *Limiter) bucket(rule Rule) *bucket {
	key := rule.BudgetKey
	if key == "" {
		key = rule.ScopeName + "\x00" + rule.InstrumentName + "\x00" + string(rule.Key)
	}
	actual, _ := l.buckets.LoadOrStore(key, &bucket{
		key:  rule.Key,
		max:  rule.Max,
		seen: make(map[string]struct{}, rule.Max),
	})
	return actual.(*bucket)
}

type bucket struct {
	key  attribute.Key
	max  int
	mu   sync.Mutex
	seen map[string]struct{}
}

// observe returns the label value to export for value, admitting it while the
// budget has room and collapsing it to OverflowValue afterwards.
//
// The contract: at most Max distinct *real* values are exported, plus the single
// shared OverflowValue bucket everything beyond the cap collapses into. Max+1
// exported series is therefore normal and always has been — the extra one is the
// overflow bucket, not a leak.
//
// A value equal to OverflowValue is deliberately *not* special-cased. A real
// label value can legitimately be the literal "other" — a Cassandra table or an
// HTTP route may be named that — and short-circuiting it used to let it skip the
// budget entirely. That broke the contract above: with Max=1, a real "other"
// followed by "/rooms" admitted *both*, exporting two real values while the
// budget was never even exhausted. Running it through the normal path costs it a
// slot and restores the guarantee.
//
// The overflow label's spelling does not move — "other" is returned whether a
// value was admitted or collapsed — so queries grouping on that literal keep
// matching. Which real values survive the cap does change, and that is the fix
// working: in the example above "/rooms" now overflows instead of taking a slot
// the sentinel failed to consume.
//
// What this does not fix: a real "other" is still indistinguishable from the
// overflow bucket once the cap is reached, and their samples merge. That is
// inherent to an in-band sentinel and needs an out-of-band representation to
// resolve, which would change an exported label value. Tracked separately.
func (b *bucket) observe(value string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[value]; ok {
		return value
	}
	if len(b.seen) >= b.max {
		return OverflowValue
	}
	b.seen[value] = struct{}{}
	return value
}

// Exporter wraps an OTel metric Exporter and rewrites capped attributes before
// export.
type Exporter struct {
	sdkmetric.Exporter
	limiter *Limiter
}

// NewExporter returns an Exporter wrapper for inner.
func NewExporter(inner sdkmetric.Exporter, rules ...Rule) *Exporter {
	return &Exporter{Exporter: inner, limiter: NewLimiter(rules...)}
}

// Export rewrites capped attributes before delegating to the wrapped exporter.
func (e *Exporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	e.limiter.Rewrite(rm)
	return e.Exporter.Export(ctx, rm)
}

// Gatherer wraps a Prometheus Gatherer and rewrites capped labels before
// promhttp serializes the scrape response.
type Gatherer struct {
	inner   prometheus.Gatherer
	limiter *prometheusLimiter
}

// NewGatherer returns a Prometheus Gatherer wrapper for inner.
func NewGatherer(inner prometheus.Gatherer, rules ...PrometheusRule) *Gatherer {
	return &Gatherer{inner: inner, limiter: newPrometheusLimiter(rules...)}
}

// Gather delegates to the wrapped gatherer and rewrites capped labels.
func (g *Gatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.inner.Gather()
	g.limiter.rewrite(families)
	return families, err
}

type prometheusLimiter struct {
	buckets sync.Map
	rules   []PrometheusRule
}

func newPrometheusLimiter(rules ...PrometheusRule) *prometheusLimiter {
	usable := make([]PrometheusRule, 0, len(rules))
	for _, rule := range rules {
		if rule.MetricName == "" || rule.LabelName == "" || rule.Max <= 0 {
			continue
		}
		usable = append(usable, rule)
	}
	return &prometheusLimiter{rules: usable}
}

func (l *prometheusLimiter) rewrite(families []*dto.MetricFamily) {
	if l == nil || len(l.rules) == 0 {
		return
	}
	for _, family := range families {
		for _, rule := range l.rules {
			if family.GetName() != rule.MetricName {
				continue
			}
			bucket := l.bucket(rule)
			for _, metric := range family.Metric {
				// Scope is matched per series, not per family: one family can
				// carry series from several instrumentation scopes.
				if rule.ScopeName != "" && prometheusLabel(metric, scopeNameLabel) != rule.ScopeName {
					continue
				}
				rewritePrometheusLabels(metric, rule.LabelName, bucket)
			}
			family.Metric = mergePrometheusMetrics(family.Metric)
		}
	}
}

// prometheusLabel returns the value of one label on a series, or "" when absent.
func prometheusLabel(metric *dto.Metric, name string) string {
	for _, label := range metric.Label {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

func (l *prometheusLimiter) bucket(rule PrometheusRule) *bucket {
	key := rule.BudgetKey
	if key == "" {
		key = rule.ScopeName + "\x00" + rule.MetricName + "\x00" + rule.LabelName
	}
	actual, _ := l.buckets.LoadOrStore(key, &bucket{
		key:  attribute.Key(rule.LabelName),
		max:  rule.Max,
		seen: make(map[string]struct{}, rule.Max),
	})
	return actual.(*bucket)
}

func rewritePrometheusLabels(metric *dto.Metric, labelName string, bucket *bucket) {
	for _, label := range metric.Label {
		if label.GetName() != labelName {
			continue
		}
		next := bucket.observe(label.GetValue())
		label.Value = stringPtr(next)
		return
	}
}

func mergePrometheusMetrics(in []*dto.Metric) []*dto.Metric {
	index := make(map[string]int, len(in))
	out := make([]*dto.Metric, 0, len(in))
	for _, metric := range in {
		key := prometheusMetricKey(metric)
		if i, ok := index[key]; ok {
			mergePrometheusMetric(out[i], metric)
			continue
		}
		index[key] = len(out)
		out = append(out, metric)
	}
	return out
}

func prometheusMetricKey(metric *dto.Metric) string {
	labels := make([]string, 0, len(metric.Label))
	for _, label := range metric.Label {
		labels = append(labels, label.GetName()+"="+label.GetValue())
	}
	sort.Strings(labels)
	return strings.Join(labels, "\xff")
}

func mergePrometheusMetric(dst, src *dto.Metric) {
	if src.Counter != nil && dst.Counter != nil {
		dst.Counter.Value = float64Ptr(dst.Counter.GetValue() + src.Counter.GetValue())
	}
	if src.Gauge != nil {
		dst.Gauge = src.Gauge
		dst.TimestampMs = src.TimestampMs
	}
	if src.Histogram != nil && dst.Histogram != nil {
		mergePrometheusHistogram(dst.Histogram, src.Histogram)
	}
	if src.Untyped != nil && dst.Untyped != nil {
		dst.Untyped.Value = float64Ptr(dst.Untyped.GetValue() + src.Untyped.GetValue())
	}
	if src.Summary != nil {
		dst.Summary = src.Summary
		dst.TimestampMs = src.TimestampMs
	}
}

func mergePrometheusHistogram(dst, src *dto.Histogram) {
	dst.SampleCount = uint64Ptr(dst.GetSampleCount() + src.GetSampleCount())
	dst.SampleSum = float64Ptr(dst.GetSampleSum() + src.GetSampleSum())
	buckets := make(map[float64]*dto.Bucket, len(dst.Bucket))
	for _, bucket := range dst.Bucket {
		buckets[bucket.GetUpperBound()] = bucket
	}
	for _, srcBucket := range src.Bucket {
		dstBucket, ok := buckets[srcBucket.GetUpperBound()]
		if !ok {
			dst.Bucket = append(dst.Bucket, srcBucket)
			continue
		}
		dstBucket.CumulativeCount = uint64Ptr(dstBucket.GetCumulativeCount() + srcBucket.GetCumulativeCount())
		if dstBucket.Exemplar == nil {
			dstBucket.Exemplar = srcBucket.Exemplar
		}
	}
	sort.Slice(dst.Bucket, func(i, j int) bool {
		return dst.Bucket[i].GetUpperBound() < dst.Bucket[j].GetUpperBound()
	})
}

func rewriteMetric(m *metricdata.Metrics, bucket *bucket, key attribute.Key) {
	switch data := m.Data.(type) {
	case metricdata.Histogram[int64]:
		points := make([]metricdata.HistogramDataPoint[int64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneHistogramDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeHistograms(points)
		m.Data = data
	case metricdata.Histogram[float64]:
		points := make([]metricdata.HistogramDataPoint[float64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneHistogramDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeHistograms(points)
		m.Data = data
	case metricdata.Sum[int64]:
		points := make([]metricdata.DataPoint[int64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeSums(points)
		m.Data = data
	case metricdata.Sum[float64]:
		points := make([]metricdata.DataPoint[float64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeSums(points)
		m.Data = data
	case metricdata.Gauge[int64]:
		points := make([]metricdata.DataPoint[int64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeGauges(points)
		m.Data = data
	case metricdata.Gauge[float64]:
		points := make([]metricdata.DataPoint[float64], len(data.DataPoints))
		for i := range data.DataPoints {
			points[i] = cloneDataPoint(data.DataPoints[i])
			points[i].Attributes = rewriteSet(points[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeGauges(points)
		m.Data = data
	}
}

func rewriteSet(set attribute.Set, key attribute.Key, bucket *bucket) attribute.Set {
	kvs := make([]attribute.KeyValue, 0, set.Len())
	changed := false
	iter := set.Iter()
	for iter.Next() {
		kv := iter.Attribute()
		if kv.Key == key {
			next := bucket.observe(kv.Value.AsString())
			changed = changed || next != kv.Value.AsString()
			kv = key.String(next)
		}
		kvs = append(kvs, kv)
	}
	if !changed {
		return set
	}
	return attribute.NewSet(kvs...)
}

func mergeHistograms[N int64 | float64](in []metricdata.HistogramDataPoint[N]) []metricdata.HistogramDataPoint[N] {
	index := make(map[attribute.Distinct]int, len(in))
	out := make([]metricdata.HistogramDataPoint[N], 0, len(in))
	for _, dp := range in {
		distinct := dp.Attributes.Equivalent()
		if i, ok := index[distinct]; ok {
			mergeHistogram(&out[i], dp)
			continue
		}
		index[distinct] = len(out)
		out = append(out, dp)
	}
	return out
}

func cloneHistogramDataPoint[N int64 | float64](dp metricdata.HistogramDataPoint[N]) metricdata.HistogramDataPoint[N] {
	dp.Bounds = append([]float64(nil), dp.Bounds...)
	dp.BucketCounts = append([]uint64(nil), dp.BucketCounts...)
	dp.Exemplars = cloneExemplars(dp.Exemplars)
	return dp
}

func mergeHistogram[N int64 | float64](dst *metricdata.HistogramDataPoint[N], src metricdata.HistogramDataPoint[N]) {
	dst.Count += src.Count
	dst.Sum += src.Sum
	for i := range src.BucketCounts {
		if i < len(dst.BucketCounts) {
			dst.BucketCounts[i] += src.BucketCounts[i]
		}
	}
	if v, ok := src.Min.Value(); ok {
		if cur, has := dst.Min.Value(); !has || v < cur {
			dst.Min = metricdata.NewExtrema(v)
		}
	}
	if v, ok := src.Max.Value(); ok {
		if cur, has := dst.Max.Value(); !has || v > cur {
			dst.Max = metricdata.NewExtrema(v)
		}
	}
	dst.Exemplars = append(dst.Exemplars, src.Exemplars...)
}

func cloneDataPoint[N int64 | float64](dp metricdata.DataPoint[N]) metricdata.DataPoint[N] {
	dp.Exemplars = cloneExemplars(dp.Exemplars)
	return dp
}

func mergeSums[N int64 | float64](in []metricdata.DataPoint[N]) []metricdata.DataPoint[N] {
	index := make(map[attribute.Distinct]int, len(in))
	out := make([]metricdata.DataPoint[N], 0, len(in))
	for _, dp := range in {
		distinct := dp.Attributes.Equivalent()
		if i, ok := index[distinct]; ok {
			out[i].Value += dp.Value
			out[i].Exemplars = append(out[i].Exemplars, dp.Exemplars...)
			continue
		}
		index[distinct] = len(out)
		out = append(out, dp)
	}
	return out
}

func mergeGauges[N int64 | float64](in []metricdata.DataPoint[N]) []metricdata.DataPoint[N] {
	index := make(map[attribute.Distinct]int, len(in))
	out := make([]metricdata.DataPoint[N], 0, len(in))
	for _, dp := range in {
		distinct := dp.Attributes.Equivalent()
		if i, ok := index[distinct]; ok {
			out[i] = dp
			continue
		}
		index[distinct] = len(out)
		out = append(out, dp)
	}
	return out
}

func cloneExemplars[N int64 | float64](exemplars []metricdata.Exemplar[N]) []metricdata.Exemplar[N] {
	if len(exemplars) == 0 {
		return nil
	}
	out := make([]metricdata.Exemplar[N], len(exemplars))
	for i, exemplar := range exemplars {
		exemplar.FilteredAttributes = append([]attribute.KeyValue(nil), exemplar.FilteredAttributes...)
		exemplar.SpanID = append([]byte(nil), exemplar.SpanID...)
		exemplar.TraceID = append([]byte(nil), exemplar.TraceID...)
		out[i] = exemplar
	}
	return out
}

func stringPtr(v string) *string {
	return &v
}

func float64Ptr(v float64) *float64 {
	return &v
}

func uint64Ptr(v uint64) *uint64 {
	return &v
}
