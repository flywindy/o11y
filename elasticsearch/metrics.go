package elasticsearch

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// instruments holds the metric instruments owned by this package.
//
// operationDuration is the same db.client.operation.duration histogram emitted
// by the MongoDB (ADR 0014), Redis (ADR 0013), and Cassandra (ADR 0019)
// integrations. One sample is recorded per request at Close — spanning retries
// and the product check, like the span — with the bounded label set built in
// metricAttrs (ADR 0027 §3).
type instruments struct {
	operationDuration metric.Float64Histogram
}

func newInstruments(meter metric.Meter) (*instruments, error) {
	operationDuration, err := meter.Float64Histogram(
		"db.client.operation.duration",
		metric.WithDescription("Duration of database client operations."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch: create operation duration histogram: %w", err)
	}
	return &instruments{operationDuration: operationDuration}, nil
}
