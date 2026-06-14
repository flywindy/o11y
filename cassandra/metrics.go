package cassandra

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// instruments holds the metric instruments owned by this package.
//
// operationDuration is the same db.client.operation.duration histogram emitted
// by the MongoDB (ADR 0014) and Redis (ADR 0013) integrations. attempts is the
// Cassandra-unique retry/speculative-execution counter (ADR 0019 §7.B): it is
// incremented by a fixed 1 per ObserveQuery callback, because gocql fires that
// callback exactly once per attempt and per page. connectDuration and
// connectCount cover the optional connect-observer signals (§7.C).
type instruments struct {
	operationDuration metric.Float64Histogram
	attempts          metric.Int64Counter
	connectDuration   metric.Float64Histogram
	connectCount      metric.Int64Counter
}

func newInstruments(meter metric.Meter) (*instruments, error) {
	operationDuration, err := meter.Float64Histogram(
		"db.client.operation.duration",
		metric.WithDescription("Duration of database client operations."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("cassandra: create operation duration histogram: %w", err)
	}
	attempts, err := meter.Int64Counter(
		"cassandra.query.attempts",
		metric.WithDescription("Number of Cassandra query attempts (one per driver attempt and per page, including retries and speculative executions)."),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return nil, fmt.Errorf("cassandra: create attempts counter: %w", err)
	}
	connectDuration, err := meter.Float64Histogram(
		"db.client.connection.create_time",
		metric.WithDescription("The time it took to create a new connection."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("cassandra: create connection create_time histogram: %w", err)
	}
	connectCount, err := meter.Int64Counter(
		"cassandra.connection.attempts",
		metric.WithDescription("Number of Cassandra connection attempts observed by the driver."),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return nil, fmt.Errorf("cassandra: create connection attempts counter: %w", err)
	}
	return &instruments{
		operationDuration: operationDuration,
		attempts:          attempts,
		connectDuration:   connectDuration,
		connectCount:      connectCount,
	}, nil
}
