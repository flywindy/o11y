package minio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// statFixture is a fake S3 endpoint that answers every request with 404, so a
// StatObject call ends in one logical span and one histogram sample. It
// counts the requests that carry a traceparent header.
type statFixture struct {
	url         string
	traceparent atomic.Int64
}

func newStatFixture(t *testing.T) *statFixture {
	t.Helper()
	f := &statFixture{}
	// The same responses as TestStatObject_404_GeneratesErrorSpan: minio-go
	// sends HEAD for StatObject, then GET for the structured error body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" {
			f.traceparent.Add(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
				`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
				`<Resource>/media/videos/a.mp4</Resource><RequestId>1</RequestId><HostId>test</HostId></Error>`))
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

// stat runs one StatObject for media/videos/a.mp4 through a client built with
// opts and returns the ended spans.
func (f *statFixture) stat(t *testing.T, mp *sdkmetric.MeterProvider, minioOpts *miniogo.Options, opts ...Option) []sdktrace.ReadOnlySpan {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	// An explicit sampler: without one the SDK reads OTEL_TRACES_SAMPLER.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	if mp == nil {
		mp = sdkmetric.NewMeterProvider()
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	}
	o := miniogo.Options{}
	if minioOpts != nil {
		o = *minioOpts
	}
	o.Creds = credentials.NewStaticV4("key", "secret", "")

	client, err := New(stripScheme(f.url), &o, tp, mp, propagation.TraceContext{}, opts...)
	require.NoError(t, err)
	_, err = client.StatObject(context.Background(), "media", "videos/a.mp4", miniogo.StatObjectOptions{})
	require.Error(t, err)
	return recorder.Ended()
}

// logicalSpan returns the one INTERNAL/CLIENT span named for the operation.
func logicalSpan(t *testing.T, spans []sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if _, ok := attrMap(s.Attributes())["object_store.operation.name"]; ok {
			return s
		}
	}
	t.Fatalf("no logical operation span among %d spans", len(spans))
	return nil
}

func TestWithObjectKeyAttribute(t *testing.T) {
	f := newStatFixture(t)

	attrs := attrMap(logicalSpan(t, f.stat(t, nil, nil)).Attributes())
	assert.Equal(t, "videos/a.mp4", attrs["object_store.object.key"], "the key is on the span by default")

	attrs = attrMap(logicalSpan(t, f.stat(t, nil, nil, WithObjectKeyAttribute(false))).Attributes())
	assert.NotContains(t, attrs, "object_store.object.key")
	assert.Equal(t, "media", attrs["object_store.bucket.name"], "only the key is withheld")
}

func TestWithAWSS3CompatAttributes(t *testing.T) {
	f := newStatFixture(t)

	attrs := attrMap(logicalSpan(t, f.stat(t, nil, nil)).Attributes())
	assert.NotContains(t, attrs, "aws.s3.bucket", "the AWS aliases are off by default")
	assert.NotContains(t, attrs, "aws.s3.key")

	attrs = attrMap(logicalSpan(t, f.stat(t, nil, nil, WithAWSS3CompatAttributes(true))).Attributes())
	assert.Equal(t, "media", attrs["aws.s3.bucket"])
	assert.Equal(t, "videos/a.mp4", attrs["aws.s3.key"])
	assert.Equal(t, "media", attrs["object_store.bucket.name"], "the aliases are dual-emitted, not a replacement")
	assert.Equal(t, "videos/a.mp4", attrs["object_store.object.key"])

	attrs = attrMap(logicalSpan(t, f.stat(t, nil, nil, WithAWSS3CompatAttributes(true), WithObjectKeyAttribute(false))).Attributes())
	assert.Equal(t, "media", attrs["aws.s3.bucket"])
	assert.NotContains(t, attrs, "aws.s3.key", "withholding the key withholds its alias too")
}

func TestWithSpanNameFormatter(t *testing.T) {
	f := newStatFixture(t)

	span := logicalSpan(t, f.stat(t, nil, nil, WithSpanNameFormatter(func(operation, bucket, key string) string {
		return operation + " " + bucket + "/" + key
	})))
	assert.Equal(t, "StatObject media/videos/a.mp4", span.Name(), "a custom name is used verbatim")

	span = logicalSpan(t, f.stat(t, nil, nil, WithSpanNameFormatter(func(string, string, string) string { return "" })))
	assert.Equal(t, "s3.StatObject media", span.Name(), "an empty name falls back to the default")
}

// countingTransport records that it carried a request, so a test can see the
// caller's transport kept as the base under the child-span wrapper.
type countingTransport struct {
	calls atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestWithHTTPChildSpans(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		f := newStatFixture(t)
		spans := f.stat(t, nil, nil)
		require.Len(t, spans, 1, "only the logical operation span")
		assert.Zero(t, f.traceparent.Load(), "no trace context travels toward the object store")
	})

	t.Run("on", func(t *testing.T) {
		f := newStatFixture(t)
		base := &countingTransport{}
		spans := f.stat(t, nil, &miniogo.Options{Transport: base}, WithHTTPChildSpans(true))

		parent := logicalSpan(t, spans)
		var children int
		for _, s := range spans {
			if s.SpanKind() == trace.SpanKindClient && s.Parent().SpanID() == parent.SpanContext().SpanID() {
				children++
			}
		}
		assert.Positive(t, children, "each HTTP round trip becomes a child of the logical span")
		assert.Positive(t, base.calls.Load(), "the caller's transport stays the base the wrapper sends through")
		assert.Zero(t, f.traceparent.Load(), "no trace context travels toward the object store")
	})
}

// TestMetricViews registers the views on a MeterProvider with distinctive
// buckets and checks the operation histogram takes them: the view matched the
// facade's instrument by name and scope.
func TestMetricViews(t *testing.T) {
	buckets := []float64{0.123, 4.56}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(MetricViews(buckets)...))

	newStatFixture(t).stat(t, mp, nil)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "minio.client.operation.duration" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.NotEmpty(t, hist.DataPoints)
			assert.Equal(t, buckets, hist.DataPoints[0].Bounds)
			found = true
		}
	}
	assert.True(t, found, "minio.client.operation.duration must be recorded")
}
