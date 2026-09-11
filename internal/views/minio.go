package views

import (
	"go.opentelemetry.io/otel/attribute"
	sdkinstrumentation "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// MinioScope is the instrumentation scope the minio package records its metrics
// under. It is defined here, not in the minio package, so the root package can
// name the scope without importing the minio-go client (ADR 0026 Option A); the
// minio package's instrumentationName is an alias of this constant, so the two
// cannot drift.
const MinioScope = "github.com/flywindy/o11y/minio"

// The two ADR 0018 §4 attribute keys the Minio view has to name. They live here
// rather than beside the rest of the object_store.* namespace in minio/client.go
// for the same reason as MinioScope, and the minio package aliases them, so the
// keys the facade records and the keys its view allows are one definition.
const (
	ObjectStoreOperationNameKey = attribute.Key("object_store.operation.name")
	ObjectStoreBucketNameKey    = attribute.Key("object_store.bucket.name")
)

// Minio returns the metric views that bound the cardinality of
// minio.client.operation.duration and pin its histogram buckets to the SDK's
// WithHistogramBuckets policy.
//
// OTel Go Views are fixed at MeterProvider construction and cannot be
// retro-fitted at instrument creation, so this slice is composed by o11y.Init
// via the ExtraViews seam. Services that build their own MeterProvider must
// register the same views via sdkmetric.WithView(...) at construction;
// otherwise the allowlist is not in effect.
//
// The view is scoped to the minio instrumentation scope so it never matches
// another integration's operation-duration instrument.
func Minio(histogramBuckets []float64) []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{
				Name:  "minio.client.operation.duration",
				Scope: sdkinstrumentation.Scope{Name: MinioScope},
			},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: histogramBuckets,
				},
				AttributeFilter: attribute.NewAllowKeysFilter(
					ObjectStoreOperationNameKey,
					ObjectStoreBucketNameKey,
					semconv.ServerAddressKey,
					semconv.ServerPortKey,
					semconv.ErrorTypeKey,
				),
			},
		),
	}
}
