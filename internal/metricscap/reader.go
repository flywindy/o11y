// Package metricscap rewrites high-cardinality metric attribute values before
// export.
package metricscap

import (
	"context"
	"sync"

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
}

// Limiter rewrites capped attributes in metricdata.ResourceMetrics.
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
		for mi := range rm.ScopeMetrics[si].Metrics {
			m := &rm.ScopeMetrics[si].Metrics[mi]
			for _, rule := range l.rulesFor(m.Name) {
				rewriteMetric(m, l.bucket(rule), rule.Key)
			}
		}
	}
}

func (l *Limiter) rulesFor(name string) []Rule {
	out := make([]Rule, 0, len(l.rules))
	for _, rule := range l.rules {
		if rule.InstrumentName == name {
			out = append(out, rule)
		}
	}
	return out
}

func (l *Limiter) bucket(rule Rule) *bucket {
	key := rule.InstrumentName + "\x00" + string(rule.Key)
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

func (b *bucket) observe(value string) string {
	if value == OverflowValue {
		return OverflowValue
	}
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

// Reader wraps an OTel metric Reader and rewrites capped attributes after
// collection.
type Reader struct {
	sdkmetric.Reader
	limiter *Limiter
}

// NewReader returns a Reader wrapper for inner.
func NewReader(inner sdkmetric.Reader, rules ...Rule) *Reader {
	return &Reader{Reader: inner, limiter: NewLimiter(rules...)}
}

// Collect delegates to the wrapped reader and rewrites capped attributes in rm.
func (r *Reader) Collect(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := r.Reader.Collect(ctx, rm); err != nil {
		return err
	}
	r.limiter.Rewrite(rm)
	return nil
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

func rewriteMetric(m *metricdata.Metrics, bucket *bucket, key attribute.Key) {
	switch data := m.Data.(type) {
	case metricdata.Histogram[int64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeHistograms(data.DataPoints)
		m.Data = data
	case metricdata.Histogram[float64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeHistograms(data.DataPoints)
		m.Data = data
	case metricdata.Sum[int64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeSums(data.DataPoints)
		m.Data = data
	case metricdata.Sum[float64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
		data.DataPoints = mergeSums(data.DataPoints)
		m.Data = data
	case metricdata.Gauge[int64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
		m.Data = data
	case metricdata.Gauge[float64]:
		for i := range data.DataPoints {
			data.DataPoints[i].Attributes = rewriteSet(data.DataPoints[i].Attributes, key, bucket)
		}
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
