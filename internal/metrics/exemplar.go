package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
)

// dropFilteredAttrsExemplarSelector is an ExemplarReservoirProviderSelector
// that wraps the SDK default but suppresses FilteredAttributes on every
// recorded exemplar. The trace_id and span_id are still attached so
// trace-to-metric linkage works in Tempo / Grafana exemplar UI.
//
// Why this exists: SDK-managed views on http.server.request.duration and
// http.client.request.duration use AttributeFilter to bound series
// cardinality. The OTel SDK routes attributes that the filter drops into
// the exemplar's FilteredAttributes instead of discarding them. The
// Prometheus exporter then encodes those as OpenMetrics exemplar labels,
// where the client_golang validator rejects any exemplar whose combined
// label runes exceed 128 (see ExemplarMaxRunes). With trace_id + span_id
// already costing 63 runes, even a couple of verbose attributes such as
// server.address or url.scheme push past the cap. otelprom's addExemplars
// then hands the error to otel.Handle and exports the sample without its
// exemplar, so the cost is the trace linkage plus one handled error per
// scrape, for as long as the series lives. Dropping FilteredAttributes at
// the reservoir keeps exemplar size constant and bounded.
//
// Suppressing them is also what keeps a key the reserved-key guard dropped
// from reaching the exposition by the exemplar route (see
// withReservedKeyFilter).
func dropFilteredAttrsExemplarSelector(agg sdkmetric.Aggregation) exemplar.ReservoirProvider {
	return dropFilteredAttrs(nil)(agg)
}

// dropFilteredAttrs returns sel with FilteredAttributes suppressed on every
// reservoir it hands out, leaving the rest of the reservoir's behaviour to
// sel. A nil sel means the SDK's default selector, so this is also how a
// stream that expressed no preference gets the guard.
//
// It composes rather than replaces for the same reason composeFilters does:
// a view owns which reservoir its stream uses, but it does not get to decide
// whether an attribute the SDK dropped may reappear on the wire. Wrapping a
// selector that already drops them is a no-op, which is what the two default
// HTTP views hit: they set this selector themselves because they are shared
// with the OTLP path, where guardReservedKeys does not run.
func dropFilteredAttrs(sel sdkmetric.ExemplarReservoirProviderSelector) sdkmetric.ExemplarReservoirProviderSelector {
	if sel == nil {
		sel = sdkmetric.DefaultExemplarReservoirProviderSelector
	}
	return func(agg sdkmetric.Aggregation) exemplar.ReservoirProvider {
		inner := sel(agg)
		return func(attrs attribute.Set) exemplar.Reservoir {
			return droppedAttrReservoir{Reservoir: inner(attrs)}
		}
	}
}

type droppedAttrReservoir struct {
	exemplar.Reservoir
}

func (r droppedAttrReservoir) Offer(ctx context.Context, t time.Time, v exemplar.Value, _ []attribute.KeyValue) {
	r.Reservoir.Offer(ctx, t, v, nil)
}
