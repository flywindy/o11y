package metrics

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// applyHistogramBucketOverrides gives each histogram named in overrides its
// own explicit bucket boundaries, in every scope that records it.
//
// A view that already claims the instrument keeps claiming it, with only its
// boundaries replaced: adding a second view there would export a second,
// conflicting stream for the same instrument and lose the claiming view's
// attribute filter. Every other instrument named in overrides gets a view of
// its own, which beats the boundaries the instrument advises, since an
// advisory applies only when no view sets an aggregation.
//
// Only explicit-bucket histograms are reshaped. A view that drops the stream,
// or picks another aggregation, is left as it is, and so is an instrument of
// another kind that happens to share the name.
func applyHistogramBucketOverrides(configured []sdkmetric.View, overrides map[string][]float64) []sdkmetric.View {
	if len(overrides) == 0 {
		return configured
	}
	out := make([]sdkmetric.View, 0, len(configured)+1)
	for _, v := range configured {
		out = append(out, withBucketOverride(v, overrides))
	}
	return append(out, unclaimedBucketOverride(configured, overrides))
}

// withBucketOverride wraps v so a histogram stream it returns for an
// overridden instrument carries the override's boundaries.
func withBucketOverride(v sdkmetric.View, overrides map[string][]float64) sdkmetric.View {
	return func(inst sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		stream, ok := v(inst)
		if !ok {
			return stream, false
		}
		buckets, found := overrides[inst.Name]
		if !found || inst.Kind != sdkmetric.InstrumentKindHistogram {
			return stream, true
		}
		switch agg := stream.Aggregation.(type) {
		case sdkmetric.AggregationExplicitBucketHistogram:
			agg.Boundaries = buckets
			stream.Aggregation = agg
		case nil, sdkmetric.AggregationDefault:
			stream.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{Boundaries: buckets}
		}
		return stream, true
	}
}

// unclaimedBucketOverride returns the view for overridden histograms no
// configured view matches. It reproduces the SDK's implicit default stream,
// as catchAllReservedKeyFilter does, with the override's boundaries set.
func unclaimedBucketOverride(configured []sdkmetric.View, overrides map[string][]float64) sdkmetric.View {
	return func(inst sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		buckets, found := overrides[inst.Name]
		if !found || inst.Kind != sdkmetric.InstrumentKindHistogram {
			return sdkmetric.Stream{}, false
		}
		for _, v := range configured {
			if _, ok := v(inst); ok {
				return sdkmetric.Stream{}, false
			}
		}
		return sdkmetric.Stream{
			Name:        inst.Name,
			Description: inst.Description,
			Unit:        inst.Unit,
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: buckets},
		}, true
	}
}
