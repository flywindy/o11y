package resty

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
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

	"github.com/flywindy/o11y/internal/redact"
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
	// secrets and authHeaderKey are what only beforeRequest can see: resty
	// passes the invoking client to no other hook, and the client is where a
	// token, a basic-auth pair and the name of the header they go in are
	// configured. They are resolved once, at the same moment the target is and
	// for the same reason.
	secrets       []string
	authHeaderKey string
	// jar is the client's cookie jar, for the cookies net/http adds to a
	// request it built itself. A redirect's request is not the one resty
	// holds, so the jar is the only place those cookies can be read from.
	jar http.CookieJar
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

	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
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
		attempt:    req.Attempt,
		retryCount: c.RetryCount,
		// Resolve the target now, while the invoking client is in hand. This
		// is the only hook resty passes it to, and Client.Clone() shallow-copies
		// the hook slice, so a clone that changes BaseURL shares this hook —
		// a client reference captured at Wrap time would be the wrong one.
		// Later stages upgrade this from RawRequest once resty has built it.
		target:        requestTarget(c, req),
		secrets:       clientSecrets(c),
		authHeaderKey: c.HeaderAuthorizationKey,
		jar:           clientJar(c),
	}
	ctx = context.WithValue(ctx, stateKey{}, state)
	state.ctx = ctx
	req.SetContext(ctx)
	h.prop.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return nil
}

func (h *hook) success(c *restyclient.Client, resp *restyclient.Response) {
	if resp == nil || resp.Request == nil {
		return
	}
	state := stateFromContext(resp.Request.Context())
	if state == nil || state.finished {
		return
	}
	// Reaching the success hook means resty made no retry decision for this
	// attempt: any decision fires the retry hook first, which ends the span.
	h.finishResponse(c, resp, state, false)
}

// finishResponse ends the attempt span for a request that produced a response.
// retryable reports whether resty decided this attempt warranted a retry; it is
// passed in rather than re-derived because the wrapper cannot see the full
// condition set (see hook.retry).
func (h *hook) finishResponse(
	c *restyclient.Client,
	resp *restyclient.Response,
	state *requestState,
	retryable bool,
) {
	state.target = resolvedTarget(c, resp.Request, state)
	statusCode := resp.StatusCode()
	attrs := append(targetAttributes(state.target), semconv.HTTPResponseStatusCode(statusCode))
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout {
		attrs = append(attrs, restyErrorKindKey.String("server_timeout"))
	}
	errType := ""
	if isErrorStatusCode(statusCode) {
		errType = strconv.Itoa(statusCode)
		attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
	}
	if isLastAttempt(state) && retryable {
		attrs = append(attrs, restyRetryExhaustedKey.Bool(true))
		addRetryExhaustedEvent(state.parentCtx)
	}

	metricAttrs := h.metricAttrs(state)
	if statusCode > 0 {
		metricAttrs = append(metricAttrs, semconv.HTTPResponseStatusCode(statusCode))
	}
	if errType != "" {
		metricAttrs = append(metricAttrs, semconv.ErrorTypeKey.String(errType))
	}

	status := codes.Unset
	description := ""
	if statusCode >= 400 {
		status = codes.Error
		description = resp.Status()
	}
	h.finish(resp.Request, state, status, description, nil, attrs, metricAttrs)
}

func isErrorStatusCode(code int) bool {
	return code >= 400 && code < 600
}

// retry runs on every retry decision resty makes, including the one taken on
// the final attempt. Reaching this hook is itself the signal that the attempt
// was retryable: resty evaluates the merged condition set — the request's own
// conditions appended to the client's (Request.AddRetryCondition writes to an
// unexported field) — so the wrapper cannot reproduce that decision. It used to
// try, via the client-level conditions alone, and a request that retried on a
// condition registered with req.AddRetryCondition therefore left every attempt
// span but the last unended and its duration sample unrecorded.
func (h *hook) retry(resp *restyclient.Response, err error) {
	if resp == nil || resp.Request == nil {
		return
	}
	state := stateFromContext(resp.Request.Context())
	if state == nil || state.finished {
		return
	}
	if err == nil {
		// resty hands the retry hook the raw operation error, which is nil when
		// the decision came from inspecting the response rather than a
		// transport failure. A response exists here, so RawRequest is built and
		// resolvedTarget needs no client.
		h.finishResponse(nil, resp, state, true)
		return
	}
	h.finishError(resp.Request, state, err, isLastAttempt(state))
}

// panicked ends the attempt span when a middleware or transport panics. resty
// unwinds through its panic hooks only, so without this the span started in
// beforeRequest would never be ended.
func (h *hook) panicked(req *restyclient.Request, err error) {
	if req == nil {
		return
	}
	state := stateFromContext(req.Context())
	if state == nil || state.finished {
		return
	}
	if err == nil {
		err = errors.New("resty: panic during request")
	}
	h.finishError(req, state, err, false)
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
	statusCode := 0
	var responseErr *restyclient.ResponseError
	if errors.As(err, &responseErr) && responseErr.Response != nil {
		statusCode = responseErr.Response.StatusCode()
		if responseErr.Response.Request != nil {
			req = responseErr.Response.Request
		}
	}
	state.target = resolvedTarget(nil, req, state)

	attrs := append(targetAttributes(state.target),
		semconv.ErrorTypeKey.String(errType),
		restyErrorKindKey.String(restyErrorKind(err)),
	)
	if statusCode > 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(statusCode))
	}
	if retryExhausted && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		attrs = append(attrs, restyRetryExhaustedKey.Bool(true))
		addRetryExhaustedEvent(state.parentCtx)
	}
	metricAttrs := append(h.metricAttrs(state), semconv.ErrorTypeKey.String(errType))
	if statusCode > 0 {
		metricAttrs = append(metricAttrs, semconv.HTTPResponseStatusCode(statusCode))
	}
	// The span's status description and its exception event carry the error's
	// text, and Go's client error is a *url.Error holding the request URL with
	// the username and the query intact. Redacting url.full while recording
	// that beside it would move the credential rather than remove it.
	h.finish(req, state, codes.Error,
		redact.Error(err, requestURLs(req), requestSecrets(req, state, err)).Error(),
		err, attrs, metricAttrs)
}

// resolvedTarget prefers the fully resolved URL resty builds into RawRequest,
// and otherwise keeps what beforeRequest resolved against the invoking client.
// The fallback is what carries server.address through an early failure — a
// panicking or erroring before-request hook runs before RawRequest exists, so
// there is nothing else left to resolve a relative URL against.
func resolvedTarget(c *restyclient.Client, req *restyclient.Request, state *requestState) targetAttrs {
	if req != nil && req.RawRequest != nil && req.RawRequest.URL != nil {
		return requestTarget(c, req)
	}
	return state.target
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
		recordRedactedError(state.span, err, requestURLs(req), requestSecrets(req, state, err))
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

// requestTarget derives the span's target attributes for one request, from
// whichever of resty's URL forms is available at the point it is called.
// finishError runs before resty has built a RawRequest, so the unresolved
// forms below are reached in practice, not only in theory.
func requestTarget(c *restyclient.Client, req *restyclient.Request) targetAttrs {
	if req == nil {
		return targetAttrs{}
	}
	if req.RawRequest != nil && req.RawRequest.URL != nil {
		return targetFromURL(req.RawRequest.URL)
	}
	rawURL := req.URL
	if parsed, err := url.Parse(rawURL); err == nil && parsed.IsAbs() {
		return targetFromURL(parsed)
	}
	if c == nil {
		return targetFromRawURL(rawURL)
	}
	base := c.BaseURL
	if base == "" {
		base = c.HostURL
	}
	if base == "" {
		return targetFromRawURL(rawURL)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return targetFromRawURL(rawURL)
	}
	rel, err := url.Parse(rawURL)
	if err != nil {
		return targetFromURL(baseURL)
	}
	return targetFromURL(baseURL.ResolveReference(rel))
}

// targetFromRawURL is the fallback for a request URL that could not be
// resolved against a base. It redacts rather than recording the raw string:
// a URL that url.Parse does not report as absolute can still carry userinfo —
// "//user:pass@host/x" is scheme-relative, so IsAbs is false while User is set
// — and a URL it cannot parse at all may be hiding a credential in any
// position, so none of it reaches the span.
func targetFromRawURL(rawURL string) targetAttrs {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return targetAttrs{}
	}
	return targetAttrs{fullURL: redact.URLAttribute(parsed)}
}

func targetAttributes(target targetAttrs) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	if target.host != "" {
		attrs = append(attrs, semconv.ServerAddress(target.host))
	}
	if target.port > 0 {
		attrs = append(attrs, semconv.ServerPort(target.port))
	}
	if target.fullURL != "" {
		attrs = append(attrs, semconv.URLFull(target.fullURL))
	}
	return attrs
}

// targetFromURL derives the server and url.full attributes for one request.
//
// The URL goes through redact.URLAttribute rather than u.String(): an outbound
// URL can carry userinfo, which Go turns into a Basic Authorization header, and
// a presigned URL carries its signature in the query, and semconv v1.39.0 says
// url.full SHOULD have both removed. otelhttp — the SDK's other client facade,
// behind o11yhttp.NewTransport — already strips userinfo before it emits
// url.full, so until now the same call was redacted through one facade and not
// the other.
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
	return targetAttrs{fullURL: redact.URLAttribute(u), host: host, port: port}
}

// recordRedactedError records err as the span's exception event with its
// credentials removed.
//
// span.RecordError cannot be used directly: it renders exception.message from
// err.Error(), which for a Go client error is a *url.Error holding the request
// URL — username and query kept, only the password masked. Passing it a
// redacted wrapper would fix the message and break exception.type, which the
// SDK derives with reflect.TypeOf, so the event is built here from the original
// error's type and the redacted text.
func recordRedactedError(span trace.Span, err error, endpoints, secrets []string) {
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(exceptionType(err)),
		semconv.ExceptionMessage(redact.Error(err, endpoints, secrets).Error()),
	))
}

// requestSecrets returns every credential this request could have put on the
// wire, in the forms something may report them.
//
// There are three sources and they are all here, rather than the one a review
// pointed at, because being covered in one place and not its sibling is the
// defect this file has produced most often:
//
//   - the request URL's userinfo, which http.Client turns into a Basic header
//     before any transport sees it (urlDerivedSecrets);
//   - the resolved request headers, which is where a caller's own
//     SetAuthToken, SetBasicAuth or Authorization ends up — resty writes all
//     three into RawRequest.Header in addCredentials, so reading that one
//     header set covers every way of configuring them;
//   - and what was configured on the request and the client but may not have
//     reached a header yet, because a before-request hook that fails or panics
//     ends the span before resty has built RawRequest at all.
//
// The client's half is in state, resolved in beforeRequest: resty hands the
// invoking client to no other hook, and Client.Clone shares the hook slice, so
// a client captured at Wrap time would be the wrong one.
func requestSecrets(req *restyclient.Request, state *requestState, err error) []string {
	var (
		secrets []string
		authKey string
	)
	if state != nil {
		secrets = append(secrets, state.secrets...)
		authKey = state.authHeaderKey
	}
	if req == nil {
		return secrets
	}
	secrets = append(secrets, urlDerivedSecrets(req)...)
	header := req.Header
	if req.RawRequest != nil && req.RawRequest.Header != nil {
		header = req.RawRequest.Header
	}
	secrets = append(secrets, redact.HeaderSecrets(header, authKey)...)
	secrets = append(secrets, configuredSecrets(req.AuthScheme, req.Token, req.UserInfo, req.Cookies)...)
	if state != nil {
		secrets = append(secrets, jarSecrets(state.jar, append(requestURLs(req), redact.ErrorURLs(err)...))...)
	}
	return secrets
}

// requestURLs names the URL this request resolved to, for the endpoint
// argument redact.Error takes. Naming it is what lets a credential net/http
// derived from *this* URL be told apart from one it derived from a redirect's
// Location, which is a URL the caller never had.
func requestURLs(req *restyclient.Request) []string {
	raw := resolvedRawURL(req)
	if raw == "" {
		return nil
	}
	return []string{raw}
}

// resolvedRawURL prefers the URL resty built into RawRequest, for the same
// reason resolvedTarget does: a relative URL carries no userinfo until it has
// been resolved against the client's base.
func resolvedRawURL(req *restyclient.Request) string {
	if req == nil {
		return ""
	}
	if req.RawRequest != nil && req.RawRequest.URL != nil {
		return req.RawRequest.URL.String()
	}
	return req.URL
}

// clientJar returns the cookie jar the invoking client will send from, read at
// the one moment resty hands the client over.
func clientJar(c *restyclient.Client) http.CookieJar {
	if c == nil {
		return nil
	}
	if hc := c.GetClient(); hc != nil {
		return hc.Jar
	}
	return nil
}

// jarSecrets returns the cookies the jar would send to each of these URLs, in
// the forms something may report them.
//
// net/http builds its own request for a redirect and fills its Cookie header
// from the jar — including a cookie the redirect response itself set — so a
// session cookie can reach the wire without ever appearing on the request
// resty holds. Asking the jar about the URLs the error names is what closes
// that: by the time an error arrives the jar holds exactly what was sent.
func jarSecrets(jar http.CookieJar, rawURLs []string) []string {
	if jar == nil {
		return nil
	}
	var secrets []string
	for _, raw := range rawURLs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			continue
		}
		for _, cookie := range jar.Cookies(parsed) {
			if cookie == nil || cookie.Value == "" {
				continue
			}
			secrets = append(secrets, redact.HeaderValueForms(cookie.Value)...)
			secrets = append(secrets, redact.HeaderValueForms(cookie.Name+"="+cookie.Value)...)
			// And the form AddCookie actually sends, which drops the bytes a
			// cookie cannot carry: a jar accepts a value net/http will not send
			// verbatim, and what reaches the wire then matches neither of the two
			// above.
			secrets = append(secrets, redact.CookieRequestForms(cookie.Name, cookie.Value, cookie.Quoted)...)
		}
	}
	return secrets
}

// clientSecrets returns the credentials configured on the invoking client, the
// half of the set that stops being reachable once beforeRequest returns.
func clientSecrets(c *restyclient.Client) []string {
	if c == nil {
		return nil
	}
	secrets := redact.HeaderSecrets(c.Header, c.HeaderAuthorizationKey)
	return append(secrets, configuredSecrets(c.AuthScheme, c.Token, c.UserInfo, c.Cookies)...)
}

// configuredSecrets renders the credentials resty's addCredentials builds a
// header out of, in the forms something may report them.
//
// The token is listed both bare and with its scheme, because a report may name
// either the header value resty composed or the value the caller configured.
// The basic pair is listed as the base64 http.Request.SetBasicAuth encodes —
// which holds neither half as a substring, so nothing else in the list would
// match it — and as the password on its own.
//
// The username is deliberately not a secret here. It is an identity rather
// than a credential, it is frequently an ordinary word, and redact.Secrets
// would replace that word everywhere it appeared in the message. Where it is
// part of a URL it is already replaced, by URLAttribute, which can tell the
// difference from position.
func configuredSecrets(scheme, token string, user *restyclient.User, cookies []*http.Cookie) []string {
	var secrets []string
	add := func(v string) {
		if v != "" {
			secrets = append(secrets, redact.HeaderValueForms(v)...)
		}
	}
	if token != "" {
		add(token)
		add(strings.TrimSpace(scheme + " " + token))
	}
	if user != nil {
		basic := redact.BasicAuthValue(user.Username, user.Password)
		add(basic)
		add(strings.TrimPrefix(basic, "Basic "))
		add(user.Password)
	}
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		add(cookie.Value)
		// The same sanitisation applies here: resty hands these to AddCookie
		// like any other, so a configured value holding a byte a cookie cannot
		// carry goes out as something the configured form does not match.
		secrets = append(secrets, redact.CookieRequestForms(cookie.Name, cookie.Value, cookie.Quoted)...)
	}
	return secrets
}

// urlDerivedSecrets returns the Authorization value net/http derives from the
// request URL's userinfo, in the forms something may report it.
//
// A transport does not see "user:pass@host": http.Client has already turned it
// into "Authorization: Basic base64(user:pass)" by the time RoundTrip is
// called, so a RoundTripper that names the header it was given — a proxy
// wrapper, an auth middleware, a retry logger — reports a credential in a form
// no URL redaction can recognise. It contains neither half as a substring, and
// it is not a URL, so neither URLAttribute nor InText's closed rules see
// anything to act on. It has to be named as a secret.
//
// The resolved URL is preferred over the request's own, for the same reason
// resolvedTarget prefers it: a relative URL carries no userinfo until it has
// been resolved against the client's base.
func urlDerivedSecrets(req *restyclient.Request) []string {
	if req == nil {
		return nil
	}
	basic := redact.BasicAuthHeader(resolvedRawURL(req))
	if basic == "" {
		return nil
	}
	// The bare token too, for a report that names only that.
	return []string{basic, strings.TrimPrefix(basic, "Basic ")}
}

// exceptionType names err's type the way span.RecordError would have.
//
// It mirrors the pinned OTel SDK's typeStr (sdk/trace/span.go): a named type
// is reported with its full import path, which is what semconv asks for and
// what type-based grouping in a backend keys on. %T would report the short
// package name instead, so an error type that is not a pointer would group
// differently here than through every other RecordError in the process.
func exceptionType(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return ""
	}
	if t.PkgPath() == "" && t.Name() == "" {
		// A pointer, or a builtin: neither has a path of its own to report.
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
}
