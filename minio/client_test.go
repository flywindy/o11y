package minio

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkmetricdata "go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		opts     *miniogo.Options
		wantErr  string
	}{
		{name: "empty endpoint", endpoint: "", opts: &miniogo.Options{}, wantErr: "endpoint must not be empty"},
		{name: "nil options", endpoint: "minio.internal:9000", opts: nil, wantErr: "options must not be nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.endpoint, tc.opts, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		secure   bool
		wantHost string
		wantPort int
	}{
		{name: "host:port", endpoint: "minio.internal:9000", secure: false, wantHost: "minio.internal", wantPort: 9000},
		{name: "host:port secure", endpoint: "minio.internal:9000", secure: true, wantHost: "minio.internal", wantPort: 9000},
		{name: "no port insecure", endpoint: "play.min.io", secure: false, wantHost: "play.min.io", wantPort: 80},
		{name: "no port secure", endpoint: "play.min.io", secure: true, wantHost: "play.min.io", wantPort: 443},
		{name: "empty", endpoint: "", secure: true, wantHost: "", wantPort: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := splitHostPort(tc.endpoint, tc.secure)
			if host != tc.wantHost || port != tc.wantPort {
				t.Fatalf("got %s:%d, want %s:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestErrorTypeAndKind(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantType string
		wantKind string
	}{
		{
			name:     "nil",
			err:      nil,
			wantType: "",
			wantKind: "",
		},
		{
			name:     "context canceled",
			err:      context.Canceled,
			wantType: "context.Canceled",
			wantKind: "client_canceled",
		},
		{
			name:     "context deadline exceeded",
			err:      context.DeadlineExceeded,
			wantType: "context.DeadlineExceeded",
			wantKind: "client_timeout",
		},
		{
			name:     "NoSuchKey",
			err:      miniogo.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound},
			wantType: "NoSuchKey",
			wantKind: "not_found",
		},
		{
			name:     "NoSuchBucket",
			err:      miniogo.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound},
			wantType: "NoSuchBucket",
			wantKind: "not_found",
		},
		{
			name:     "AccessDenied",
			err:      miniogo.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden},
			wantType: "AccessDenied",
			wantKind: "access_denied",
		},
		{
			name:     "SignatureDoesNotMatch",
			err:      miniogo.ErrorResponse{Code: "SignatureDoesNotMatch", StatusCode: http.StatusForbidden},
			wantType: "SignatureDoesNotMatch",
			wantKind: "access_denied",
		},
		{
			name:     "PreconditionFailed",
			err:      miniogo.ErrorResponse{Code: "PreconditionFailed", StatusCode: http.StatusPreconditionFailed},
			wantType: "PreconditionFailed",
			wantKind: "precondition",
		},
		{
			name:     "SlowDown",
			err:      miniogo.ErrorResponse{Code: "SlowDown", StatusCode: http.StatusServiceUnavailable},
			wantType: "SlowDown",
			wantKind: "throttled",
		},
		{
			name:     "5xx with code",
			err:      miniogo.ErrorResponse{Code: "InternalError", StatusCode: http.StatusInternalServerError},
			wantType: "InternalError",
			wantKind: "server_error",
		},
		{
			name:     "transport / net.OpError",
			err:      &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			wantType: "*net.OpError",
			wantKind: "transport",
		},
		{
			name:     "unknown",
			err:      errors.New("something weird"),
			wantType: "*errors.errorString",
			wantKind: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorType(tc.err); got != tc.wantType {
				t.Errorf("errorType = %q, want %q", got, tc.wantType)
			}
			if got := minioErrorKind(tc.err); got != tc.wantKind {
				t.Errorf("minioErrorKind = %q, want %q", got, tc.wantKind)
			}
		})
	}
}

// TestStatObject_404_GeneratesErrorSpan exercises the full wrapper against
// a fake S3 endpoint that returns 404 for HEAD, verifying that the logical
// span carries the documented error-path attributes.
func TestStatObject_404_GeneratesErrorSpan(t *testing.T) {
	errBody := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Resource>/media/videos/missing.mp4</Resource>` +
		`<RequestId>16B7DEC3</RequestId><HostId>test</HostId></Error>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// minio-go does a HEAD first for StatObject and re-issues GET to
		// fetch the structured error body. Return XML on the GET path
		// and the 404 status everywhere so both code paths resolve to
		// the same parsed ErrorResponse.
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-amz-request-id", "16B7DEC3")
		w.WriteHeader(http.StatusNotFound)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(errBody))
		}
	}))
	t.Cleanup(server.Close)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))

	endpoint := stripScheme(server.URL)
	client, err := New(endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4("key", "secret", ""),
		Secure: false,
	}, tp, mp, propagation.NewCompositeTextMapPropagator())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.StatObject(context.Background(), "media", "videos/missing.mp4", miniogo.StatObjectOptions{})
	if err == nil {
		t.Fatal("StatObject: want error, got nil")
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "StatObject media" {
		t.Errorf("span name = %q, want %q", s.Name(), "StatObject media")
	}
	if s.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", s.Status().Code)
	}
	attrs := attrMap(s.Attributes())
	mustAttr(t, attrs, "object_store.system.name", "s3")
	mustAttr(t, attrs, "object_store.operation.name", "StatObject")
	mustAttr(t, attrs, "object_store.bucket.name", "media")
	mustAttr(t, attrs, "object_store.object.key", "videos/missing.mp4")
	mustAttr(t, attrs, "minio.error.kind", "not_found")
	mustAttr(t, attrs, "error.type", "NoSuchKey")

	var collected sdkmetricdata.ResourceMetrics
	if err := mr.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if !hasMetric(collected, "minio.client.operation.duration") {
		t.Errorf("metric minio.client.operation.duration not recorded")
	}
}

// TestPutObject_UnknownLength_RecordsSizeFromUploadInfo verifies that
// when a caller passes objectSize = -1 (stream of unknown length), the
// wrapper folds the resolved byte count from UploadInfo.Size back onto
// the span on success — matching the FPutObject / StatObject paths so
// observers see a real size regardless of whether the caller knew it
// up front (ADR 0018 §4).
func TestPutObject_UnknownLength_RecordsSizeFromUploadInfo(t *testing.T) {
	const fakeETag = `"d41d8cd98f00b204e9800998ecf8427e"`
	// minio-go's PutObject with objectSize=-1 forces a multipart upload.
	// Implement the three endpoints it dials so the call can complete and
	// surface a real UploadInfo.Size.
	initBody := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<InitiateMultipartUploadResult><Bucket>media</Bucket>` +
		`<Key>stream</Key><UploadId>UPLOAD-1</UploadId></InitiateMultipartUploadResult>`
	completeBody := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<CompleteMultipartUploadResult><Location>http://x/media/stream</Location>` +
		`<Bucket>media</Bucket><Key>stream</Key><ETag>` + fakeETag + `</ETag>` +
		`</CompleteMultipartUploadResult>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-request-id", "TEST")
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Has("location"):
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint></LocationConstraint>`))
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(initBody))
		case r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(completeBody))
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", fakeETag)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))

	client, err := New(stripScheme(server.URL), &miniogo.Options{
		Creds:  credentials.NewStaticV4("key", "secret", ""),
		Secure: false,
	}, tp, mp, propagation.NewCompositeTextMapPropagator())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	payload := strings.Repeat("a", 1024)
	info, err := client.PutObject(context.Background(), "media", "stream", strings.NewReader(payload),
		-1, miniogo.PutObjectOptions{PartSize: 5 * 1024 * 1024})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if info.Size <= 0 {
		t.Fatalf("expected UploadInfo.Size > 0, got %d", info.Size)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	attrs := spans[0].Attributes()
	var sizeAttr *attribute.KeyValue
	for i := range attrs {
		if string(attrs[i].Key) == "object_store.object.size" {
			sizeAttr = &attrs[i]
			break
		}
	}
	if sizeAttr == nil {
		t.Fatalf("missing object_store.object.size on span; got %v", attrs)
	}
	if got := sizeAttr.Value.AsInt64(); got != info.Size {
		t.Errorf("object_store.object.size = %d, want %d (UploadInfo.Size)", got, info.Size)
	}
}

// TestWrappedMethods_EmitOperationSpan covers the remaining exported
// wrapper methods (PutObject, FPutObject, FGetObject, GetObject,
// RemoveObject, CopyObject, ListObjects). It uses one in-process
// S3-shaped server that returns minimal-but-parseable responses, then
// asserts that each invocation produces a single logical span with the
// documented name and object_store.operation.name attribute, and that
// the minio.client.operation.duration histogram is recorded — the
// telemetry contract the wrapper owns.
func TestWrappedMethods_EmitOperationSpan(t *testing.T) {
	listBody := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<ListBucketResult><Name>media</Name>` +
		`<Prefix></Prefix><KeyCount>0</KeyCount>` +
		`<IsTruncated>false</IsTruncated></ListBucketResult>`
	copyBody := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<CopyObjectResult><ETag>"abc123"</ETag>` +
		`<LastModified>2026-06-04T00:00:00.000Z</LastModified></CopyObjectResult>`
	const fakeETag = `"d41d8cd98f00b204e9800998ecf8427e"`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-request-id", "TEST")
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("ETag", fakeETag)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			// List buckets responses come back via list-type query.
			if strings.Contains(r.URL.RawQuery, "list-type=") {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(listBody))
				return
			}
			w.Header().Set("ETag", fakeETag)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("payload"))
		case http.MethodPut:
			// CopyObject sends x-amz-copy-source; reply with XML.
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(copyBody))
				return
			}
			w.Header().Set("ETag", fakeETag)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	tmpDir := t.TempDir()
	uploadPath := tmpDir + "/upload.bin"
	if err := os.WriteFile(uploadPath, []byte("payload"), 0o600); err != nil {
		t.Fatalf("seed upload file: %v", err)
	}
	downloadPath := tmpDir + "/download.bin"

	cases := []struct {
		name     string
		wantSpan string
		wantOp   string
		invoke   func(t *testing.T, c *Client, ctx context.Context)
	}{
		{
			name: "PutObject", wantSpan: "PutObject media", wantOp: "PutObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				_, _ = c.PutObject(ctx, "media", "key", strings.NewReader("payload"),
					int64(len("payload")), miniogo.PutObjectOptions{})
			},
		},
		{
			name: "FPutObject", wantSpan: "FPutObject media", wantOp: "FPutObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				_, _ = c.FPutObject(ctx, "media", "key", uploadPath, miniogo.PutObjectOptions{})
			},
		},
		{
			name: "FGetObject", wantSpan: "FGetObject media", wantOp: "FGetObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				_ = c.FGetObject(ctx, "media", "key", downloadPath, miniogo.GetObjectOptions{})
			},
		},
		{
			name: "GetObject", wantSpan: "GetObject media", wantOp: "GetObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				obj, _ := c.GetObject(ctx, "media", "key", miniogo.GetObjectOptions{})
				if obj != nil {
					_ = obj.Close()
				}
			},
		},
		{
			name: "RemoveObject", wantSpan: "RemoveObject media", wantOp: "RemoveObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				_ = c.RemoveObject(ctx, "media", "key", miniogo.RemoveObjectOptions{})
			},
		},
		{
			name: "CopyObject", wantSpan: "CopyObject dst", wantOp: "CopyObject",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				_, _ = c.CopyObject(ctx,
					miniogo.CopyDestOptions{Bucket: "dst", Object: "destkey"},
					miniogo.CopySrcOptions{Bucket: "media", Object: "key"})
			},
		},
		{
			name: "ListObjects", wantSpan: "ListObjects media", wantOp: "ListObjects",
			invoke: func(t *testing.T, c *Client, ctx context.Context) {
				t.Helper()
				ch := c.ListObjects(ctx, "media", miniogo.ListObjectsOptions{})
				for range ch { //nolint:revive // empty body is the idiomatic Go drain
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			mr := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))

			client, err := New(stripScheme(server.URL), &miniogo.Options{
				Creds:  credentials.NewStaticV4("key", "secret", ""),
				Secure: false,
			}, tp, mp, propagation.NewCompositeTextMapPropagator())
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			tc.invoke(t, client, context.Background())

			spans := recorder.Ended()
			var match sdktrace.ReadOnlySpan
			for i := range spans {
				if spans[i].Name() == tc.wantSpan {
					match = spans[i]
					break
				}
			}
			if match == nil {
				names := make([]string, 0, len(spans))
				for _, s := range spans {
					names = append(names, s.Name())
				}
				t.Fatalf("want span %q, got %v", tc.wantSpan, names)
			}
			attrs := attrMap(match.Attributes())
			mustAttr(t, attrs, "object_store.system.name", "s3")
			mustAttr(t, attrs, "object_store.operation.name", tc.wantOp)

			var collected sdkmetricdata.ResourceMetrics
			if err := mr.Collect(context.Background(), &collected); err != nil {
				t.Fatalf("collect metrics: %v", err)
			}
			if !hasMetric(collected, "minio.client.operation.duration") {
				t.Errorf("metric minio.client.operation.duration not recorded")
			}
		})
	}
}

// stripScheme converts an httptest URL "http://127.0.0.1:NNN" into the
// "host:port" form minio-go expects.
func stripScheme(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	return u.Host
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

func mustAttr(t *testing.T, attrs map[string]string, key, want string) {
	t.Helper()
	got, ok := attrs[key]
	if !ok {
		t.Errorf("missing span attribute %q", key)
		return
	}
	if got != want {
		t.Errorf("span attribute %q = %q, want %q", key, got, want)
	}
}

func hasMetric(rm sdkmetricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}
