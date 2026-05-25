package resty

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
)

func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	default:
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return reflect.TypeOf(cert).String()
		}
		var host *x509.HostnameError
		if errors.As(err, &host) {
			return reflect.TypeOf(host).String()
		}
		var authority *x509.UnknownAuthorityError
		if errors.As(err, &authority) {
			return reflect.TypeOf(authority).String()
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return reflect.TypeOf(opErr).String()
		}
		if typ := reflect.TypeOf(err); typ != nil {
			return typ.String()
		}
		return "_OTHER"
	}
}

func restyErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
		return "client_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "client_timeout"
	case isTLSError(err):
		return "tls"
	case isTransportError(err):
		return "transport"
	case isProtocolError(err):
		return "protocol"
	default:
		return "unknown"
	}
}

func isTLSError(err error) bool {
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return true
	}
	var host *x509.HostnameError
	if errors.As(err, &host) {
		return true
	}
	var authority *x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return true
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil && strings.Contains(strings.ToLower(opErr.Err.Error()), "tls") {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "tls:") || strings.Contains(lower, "certificate")
}

func isTransportError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func isProtocolError(err error) bool {
	if hasTypeInChain(err, "http2.StreamError", "*http2.StreamError") {
		return true
	}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, http.ErrBodyReadAfterClose):
		return true
	default:
		lower := strings.ToLower(err.Error())
		return strings.Contains(lower, "malformed http") ||
			strings.Contains(lower, "http2:") ||
			strings.Contains(lower, "server gave http response to https client")
	}
}

func hasTypeInChain(err error, names ...string) bool {
	if err == nil {
		return false
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, name := range names {
		nameSet[name] = struct{}{}
	}
	var visit func(error) bool
	visit = func(err error) bool {
		if err == nil {
			return false
		}
		if typ := reflect.TypeOf(err); typ != nil {
			if _, ok := nameSet[typ.String()]; ok {
				return true
			}
		}
		if u, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range u.Unwrap() {
				if visit(child) {
					return true
				}
			}
			return false
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			return visit(u.Unwrap())
		}
		return false
	}
	return visit(err)
}
