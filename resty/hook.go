package resty

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	restyclient "github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	restyErrorKindKey      = attribute.Key("resty.error.kind")
	restyRetryExhaustedKey = attribute.Key("resty.retry.exhausted")
)

type stateKey struct{}

type hook struct {
	tracer   trace.Tracer
	duration metric.Float64Histogram
	prop     propagation.TextMapPropagator
	cfg      config
}

type requestState struct {
	parentCtx  context.Context
	ctx        context.Context
	span       trace.Span
	start      time.Time
	method     string
	route      string
	target     targetAttrs
	attempt    int
	retryCount int
	finished   bool
}

type targetAttrs struct {
	fullURL string
	host    string
	port    int
}

func newHook(
	tp trace.TracerProvider,
	duration metric.Float64Histogram,
	prop propagation.TextMapPropagator,
	cfg config,
) *hook {
	return &hook{
		tracer: tp.Tracer(
			instrumentationName,
			trace.WithSchemaURL(semconv.SchemaURL),
		),
		duration: duration,
		prop:     prop,
		cfg:      cfg,
	}
}

func (h *hook) beforeRequest(c *restyclient.Client, req *restyclient.Request) error {
	parentCtx := req.Context()
	if prev := stateFromContext(parentCtx); prev != nil {
		parentCtx = prev.parentCtx
	}

	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	route := h.route(parentCtx)
	target := requestTarget(c, req)

	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
	}
	if target.host != "" {
		attrs = append(attrs, semconv.ServerAddress(target.host))
	}
	if target.port > 0 {
		attrs = append(attrs, semconv.ServerPort(target.port))
	}
	if target.fullURL != "" {
		attrs = append(attrs, semconv.URLFull(target.fullURL))
	}
	if req.Attempt >= 2 {
		attrs = append(attrs, semconv.HTTPRequestResendCount(req.Attempt-1))
	}
	if route != "" {
		attrs = append(attrs, semconv.HTTPRoute(route))
	}

	ctx, span := h.tracer.Start(parentCtx, h.spanName(req, method, route),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	state := &requestState{
		parentCtx:  parentCtx,
		ctx:        ctx,
		span:       span,
		start:      time.Now(),
		method:     method,
		route:      route,
		target:     target,
		attempt:    req.Attempt,
		retryCount: c.RetryCount,
	}
	ctx = context.WithValue(ctx, stateKey{}, state)
	state.ctx = ctx
	req.SetContext(ctx)
	h.prop.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return nil
}

func (h *hook) afterResponse(c *restyclient.Client, resp *restyclient.Response) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	state := stateFromContext(resp.Request.Context())
	if state == nil || state.finished {
		return nil
	}

	statusCode := resp.StatusCode()
	attrs := []attribute.KeyValue{semconv.HTTPResponseStatusCode(statusCode)}
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout {
		attrs = append(attrs, restyErrorKindKey.String("server_timeout"))
	}
	if isLastAttempt(state) && h.retryableResponse(c, resp) {
		attrs = append(attrs, restyRetryExhaustedKey.Bool(true))
		addRetryExhaustedEvent(state.parentCtx)
	}

	metricAttrs := h.metricAttrs(state)
	if statusCode > 0 {
		metricAttrs = append(metricAttrs, semconv.HTTPResponseStatusCode(statusCode))
	}

	status := codes.Unset
	description := ""
	if statusCode >= 400 {
		status = codes.Error
		description = resp.Status()
	}
	h.finish(resp.Request, state, status, description, nil, attrs, metricAttrs)
	return nil
}

func (h *hook) retry(resp *restyclient.Response, err error) {
	if resp == nil || resp.Request == nil || err == nil {
		return
	}
	state := stateFromContext(resp.Request.Context())
	if state == nil || state.finished {
		return
	}
	h.finishError(resp.Request, state, err, isLastAttempt(state))
}

func (h *hook) error(req *restyclient.Request, err error) {
	if req == nil || err == nil {
		return
	}
	state := stateFromContext(req.Context())
	if state == nil || state.finished {
		return
	}
	h.finishError(req, state, err, isLastAttempt(state))
}

func (h *hook) finishError(req *restyclient.Request, state *requestState, err error, retryExhausted bool) {
	errType := errorType(err)
	attrs := []attribute.KeyValue{
		semconv.ErrorTypeKey.String(errType),
		restyErrorKindKey.String(restyErrorKind(err)),
	}
	if retryExhausted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		attrs = append(attrs, restyRetryExhaustedKey.Bool(true))
		addRetryExhaustedEvent(state.parentCtx)
	}
	metricAttrs := append(h.metricAttrs(state), semconv.ErrorTypeKey.String(errType))
	h.finish(req, state, codes.Error, err.Error(), err, attrs, metricAttrs)
}

func (h *hook) finish(
	req *restyclient.Request,
	state *requestState,
	status codes.Code,
	description string,
	err error,
	spanAttrs []attribute.KeyValue,
	metricAttrs []attribute.KeyValue,
) {
	if state.finished {
		return
	}
	state.finished = true
	if len(spanAttrs) > 0 {
		state.span.SetAttributes(spanAttrs...)
	}
	if err != nil {
		state.span.RecordError(err)
	}
	if status != codes.Unset {
		state.span.SetStatus(status, description)
	}
	h.duration.Record(state.ctx, time.Since(state.start).Seconds(), metric.WithAttributes(metricAttrs...))
	state.span.End()
	req.SetContext(state.parentCtx)
}

func (h *hook) metricAttrs(state *requestState) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(state.method),
	}
	if state.target.host != "" {
		attrs = append(attrs, semconv.ServerAddress(state.target.host))
	}
	if state.target.port > 0 {
		attrs = append(attrs, semconv.ServerPort(state.target.port))
	}
	if h.cfg.metricRouteEnabled && state.route != "" {
		attrs = append(attrs, semconv.HTTPRoute(state.route))
	}
	return attrs
}

func (h *hook) route(ctx context.Context) string {
	if !h.cfg.routeEnabled || ctx == nil {
		return ""
	}
	route, _ := ctx.Value(h.cfg.routeKey).(string)
	return route
}

func (h *hook) spanName(req *restyclient.Request, method, route string) string {
	if h.cfg.spanNameFormatter != nil {
		if name := h.cfg.spanNameFormatter(req); name != "" {
			return name
		}
	}
	if route != "" {
		return method + " " + route
	}
	return method
}

func (h *hook) retryableResponse(c *restyclient.Client, resp *restyclient.Response) bool {
	if c == nil || resp == nil || len(c.RetryConditions) == 0 {
		return false
	}
	for _, condition := range c.RetryConditions {
		if condition != nil && condition(resp, nil) {
			return true
		}
	}
	return false
}

func stateFromContext(ctx context.Context) *requestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(stateKey{}).(*requestState)
	return state
}

func isLastAttempt(state *requestState) bool {
	return state.retryCount > 0 && state.attempt >= state.retryCount+1
}

func addRetryExhaustedEvent(ctx context.Context) {
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		span.AddEvent("retry_exhausted", trace.WithAttributes(restyRetryExhaustedKey.Bool(true)))
	}
}

func requestTarget(c *restyclient.Client, req *restyclient.Request) targetAttrs {
	if req.RawRequest != nil && req.RawRequest.URL != nil {
		return targetFromURL(req.RawRequest.URL)
	}
	rawURL := req.URL
	if parsed, err := url.Parse(rawURL); err == nil && parsed.IsAbs() {
		return targetFromURL(parsed)
	}
	base := c.BaseURL
	if base == "" {
		base = c.HostURL
	}
	if base == "" {
		return targetAttrs{fullURL: rawURL}
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return targetAttrs{fullURL: rawURL}
	}
	rel, err := url.Parse(rawURL)
	if err != nil {
		return targetFromURL(baseURL)
	}
	return targetFromURL(baseURL.ResolveReference(rel))
}

func targetFromURL(u *url.URL) targetAttrs {
	if u == nil {
		return targetAttrs{}
	}
	host := u.Hostname()
	port := 0
	if rawPort := u.Port(); rawPort != "" {
		if parsed, err := strconv.Atoi(rawPort); err == nil {
			port = parsed
		}
	} else {
		switch u.Scheme {
		case "http":
			port = 80
		case "https":
			port = 443
		}
	}
	return targetAttrs{fullURL: u.String(), host: host, port: port}
}
