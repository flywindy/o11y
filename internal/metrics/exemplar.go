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
// already costing ~68 runes, even a couple of verbose attributes such as
// server.address or url.scheme push past the cap and the entire /metrics
// scrape fails. Dropping FilteredAttributes at the reservoir keeps
// exemplar size constant and bounded.
func dropFilteredAttrsExemplarSelector(agg sdkmetric.Aggregation) exemplar.ReservoirProvider {
	inner := sdkmetric.DefaultExemplarReservoirProviderSelector(agg)
	return func(attrs attribute.Set) exemplar.Reservoir {
		return droppedAttrReservoir{Reservoir: inner(attrs)}
	}
}

type droppedAttrReservoir struct {
	exemplar.Reservoir
}

func (r droppedAttrReservoir) Offer(ctx context.Context, t time.Time, v exemplar.Value, _ []attribute.KeyValue) {
	r.Reservoir.Offer(ctx, t, v, nil)
}
