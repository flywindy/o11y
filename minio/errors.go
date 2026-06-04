package minio

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"

	miniogo "github.com/minio/minio-go/v7"
)

// errorType returns the OTel error.type value for err, following ADR 0018
// §4: the Go type name for transport-class failures, or the S3 wire code
// from minio.ToErrorResponse(err).Code for structured S3 errors.
func errorType(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	}
	if code := s3ErrorCode(err); code != "" {
		return code
	}
	if typ := reflect.TypeOf(err); typ != nil {
		return typ.String()
	}
	return "_OTHER"
}

// minioErrorKind classifies err into the closed SRE-facing taxonomy from
// ADR 0018 §8. First match wins; the ordering matches the table.
func minioErrorKind(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
		return "client_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "client_timeout"
	}
	code := s3ErrorCode(err)
	switch code {
	case "NoSuchKey", "NoSuchBucket":
		return "not_found"
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
		return "access_denied"
	case "PreconditionFailed":
		return "precondition"
	case "SlowDown":
		return "throttled"
	}
	if status := s3StatusCode(err); status == http.StatusServiceUnavailable {
		return "throttled"
	}
	if isTLSError(err) {
		// TLS failures surface as transport in v1 — kept under the
		// transport row so the taxonomy stays bounded to nine values.
		return "transport"
	}
	if isTransportError(err) {
		return "transport"
	}
	if code != "" && s3StatusCode(err) >= 500 {
		return "server_error"
	}
	return "unknown"
}

// statusFromError extracts the HTTP status code from a structured S3 error
// when one is present; returns 0 otherwise. Used for both the
// minio.error.kind taxonomy (throttled at 503) and for the server-error
// gate (5xx).
func s3StatusCode(err error) int {
	er := minioErrorResponse(err)
	if er == nil {
		return 0
	}
	return er.StatusCode
}

func s3ErrorCode(err error) string {
	er := minioErrorResponse(err)
	if er == nil {
		return ""
	}
	return er.Code
}

func minioErrorResponse(err error) *miniogo.ErrorResponse {
	if err == nil {
		return nil
	}
	var er miniogo.ErrorResponse
	if errors.As(err, &er) {
		return &er
	}
	return nil
}

func isTLSError(err error) bool {
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return true
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil &&
		strings.Contains(strings.ToLower(opErr.Err.Error()), "tls") {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "tls:") || strings.Contains(lower, "certificate")
}

func isTransportError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}
