package log

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/flywindy/o11y/internal/redact"
	"github.com/flywindy/o11y/internal/repeat"
)

// NewLogr returns a logr.Logger that writes OpenTelemetry's internal
// diagnostics to logger as structured records, so a caller can install it
// with otel.SetLogger and stop the OTel SDK from printing plain text to
// stderr.
//
// Verbosity follows the convention the OTel Go SDK uses for its own
// messages (see go.opentelemetry.io/otel/internal/global): V(1) is a
// warning, V(4) is informational, V(8) is debug. Levels in between round to
// the next named level, so V(2) and V(3) are informational and V(5) and
// above are debug; V(0), which the SDK never uses, is informational.
//
// When suppress is non-nil each distinct message is written once per its
// window, keyed by the message text alone: the SDK repeats "dropped log
// records" with a different count on every poll while the collector is
// unreachable, and one line per poll would say nothing new. The first
// occurrence in each window carries the count it saw.
//
// endpoints are the configured export endpoints; an error that quotes one
// back (net/url does, verbatim) goes through redact.InText before it is
// logged or used as a repeat key, so embedded credentials never reach the
// log.
func NewLogr(logger *slog.Logger, suppress *repeat.Suppressor, endpoints ...string) logr.Logger {
	return NewLogrRedacting(logger, suppress, Redaction{Endpoints: endpoints})
}

// Redaction names what the sink strips from every piece of text it emits:
// Endpoints are the configured export endpoints, handled by redact.InText
// (userinfo and the endpoints' own credentials); Secrets are opaque values
// replaced wholesale by redact.Secrets, such as configured OTLP header
// values, which carry no structure the text rules could recognise and which
// the pinned exporters echo verbatim when OTEL_EXPORTER_OTLP_HEADERS fails
// to parse.
type Redaction struct {
	Endpoints []string
	Secrets   []string
}

// Text applies both rules to s, endpoints first.
func (r Redaction) Text(s string) string {
	return redact.Secrets(redact.InText(s, r.Endpoints...), r.Secrets...)
}

// NewLogrRedacting is NewLogr with the full Redaction, secrets included.
func NewLogrRedacting(logger *slog.Logger, suppress *repeat.Suppressor, r Redaction) logr.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return logr.New(&logrSink{logger: logger, suppress: suppress, now: time.Now, redaction: r})
}

// logrSink adapts logr's LogSink to slog.
type logrSink struct {
	logger    *slog.Logger
	suppress  *repeat.Suppressor
	now       func() time.Time
	redaction Redaction
	name      string
	values    []any
}

// slogLevel maps a logr verbosity to the slog level OTel's convention gives
// it.
func slogLevel(v int) slog.Level {
	switch {
	case v == 1:
		return slog.LevelWarn
	case v <= 4:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// Init implements logr.LogSink.
func (s *logrSink) Init(logr.RuntimeInfo) {}

// Enabled implements logr.LogSink.
func (s *logrSink) Enabled(level int) bool {
	return s.logger.Enabled(context.Background(), slogLevel(level))
}

// Info implements logr.LogSink. The message goes through the redaction
// rules like every value, since an OTel component may put an endpoint or a
// header value in the message text rather than in a key/value pair.
func (s *logrSink) Info(level int, msg string, keysAndValues ...any) {
	msg = s.redaction.Text(msg)
	s.write(slogLevel(level), msg, msg, keysAndValues)
}

// ErrorText renders err for a diagnostic record without letting a broken
// error value take the process down: a typed nil pointer is named rather
// than dereferenced by its own Error method, and an Error method that
// panics is recovered into a placeholder naming the type. A diagnostic is
// never worth a crash. Both the logr sink and the SDK's ErrorHandler render
// through it.
func ErrorText(err error) (text string) {
	if rv := reflect.ValueOf(err); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return fmt.Sprintf("<nil %T>", err)
	}
	defer func() {
		if r := recover(); r != nil {
			text = fmt.Sprintf("[omitted: %T panicked while rendering]", err)
		}
	}()
	return err.Error()
}

// Error implements logr.LogSink. The error text, with any configured
// endpoint's credentials redacted, is carried as the "error" attribute and
// takes part in the repeat key, so two different errors under the same
// message are each logged. The text comes from ErrorText, so a typed nil
// or a panicking Error method cannot crash the process.
func (s *logrSink) Error(err error, msg string, keysAndValues ...any) {
	msg = s.redaction.Text(msg)
	key := msg
	if err != nil {
		text := s.redaction.Text(ErrorText(err))
		key = msg + ": " + text
		keysAndValues = append([]any{slog.String("error", text)}, keysAndValues...)
	}
	s.write(slog.LevelError, key, msg, keysAndValues)
}

// write applies suppression keyed by key and emits msg with the sink's
// name, accumulated values and the caller's key/value pairs, each value
// normalized by resolve.
func (s *logrSink) write(level slog.Level, key, msg string, keysAndValues []any) {
	if s.suppress != nil && s.suppress.SuppressedAt(key, s.now()) {
		return
	}
	args := make([]any, 0, len(s.values)+len(keysAndValues)+2)
	if s.name != "" {
		args = append(args, slog.String("logger", s.name))
	}
	for _, v := range s.values {
		args = append(args, s.resolve(v))
	}
	for _, v := range keysAndValues {
		args = append(args, s.resolve(v))
	}
	if s.suppress != nil {
		args = append(args, slog.String("repeat_suppressed_for", s.suppress.Window().String()))
	}
	s.logger.Log(context.Background(), level, msg, args...)
}

// maxResolveDepth bounds the recursion through nested containers, groups
// and Marshaler results, so a Marshaler that returns another Marshaler (or
// itself) and a value nested without end cannot recurse without end. A
// value below the bound is replaced by tooDeep rather than passed through
// unchanged, so nothing unredacted reaches the log.
const maxResolveDepth = 32

// tooDeep replaces a value nested deeper than maxResolveDepth.
const tooDeep = "[omitted: nested deeper than the SDK redacts]"

// resolve prepares one logr key or value for slog. A logr.Marshaler (OTel
// passes an attribute.Set as the "attributes" value of its "Tracer created"
// diagnostic) is replaced by what MarshalLog returns, since slog would
// otherwise see only unexported fields and render "{}"; that result is then
// resolved like any other value, so the map[string]string an attribute.Set
// marshals to has its values redacted too. Strings, errors, string-valued
// slog.Attrs and the strings inside string-keyed maps, slices and arrays
// (of any element type, nested in any combination) go through
// redact.InText with the configured endpoints: otlptracehttp reports an
// endpoint that fails to parse as a "url" value beside the error, and the
// error text alone being redacted would leave the credential in that
// field. Keys are strings too and pass through unchanged in practice;
// redacting them is harmless.
func (s *logrSink) resolve(v any) any {
	return s.resolveDepth(v, 0)
}

// resolveDepth is resolve with the recursion depth tracked; see
// maxResolveDepth.
func (s *logrSink) resolveDepth(v any, depth int) (out any) {
	if depth > maxResolveDepth {
		return tooDeep
	}
	// A typed nil pointer satisfies the interfaces below, and Go does not
	// require a String, LogValue or MarshalLog method to accept a nil
	// receiver; the handler would render it as <nil> without calling any
	// of them, so do the same. A method that panics anyway (on some other
	// input) must not take the process down over a diagnostic: the value is
	// omitted and named instead.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return v
	}
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("[omitted: %T panicked while rendering]", v)
		}
	}()
	if m, ok := v.(logr.Marshaler); ok {
		return s.resolveDepth(m.MarshalLog(), depth+1)
	}
	switch t := v.(type) {
	case error:
		return s.redaction.Text(t.Error())
	case slog.Attr:
		return s.resolveAttr(t, depth)
	case slog.Value:
		return s.resolveDepth(t.Resolve().Any(), depth+1)
	case slog.LogValuer:
		// A bare LogValuer (not wrapped in an Attr) would otherwise be
		// resolved by the handler, after redaction; resolve it here so its
		// output is what gets inspected.
		return s.resolveDepth(t.LogValue().Resolve().Any(), depth+1)
	case time.Time, time.Duration:
		// Both are Stringers, but carry no text and have slog kinds of
		// their own; leave them so the handler renders them as usual.
		return v
	case url.URL:
		// A URL is handled before the generic paths: its String() carries
		// the userinfo, and walking it as a struct would reach the
		// *url.Userinfo field, whose own String() is "user:password" with
		// nothing for the text rules to anchor on.
		return s.redaction.Text(redact.URL(t.String()))
	case *url.URL:
		return s.redaction.Text(redact.URL(t.String()))
	case url.Userinfo, *url.Userinfo:
		// Userinfo on its own is a credential and nothing else; replace it
		// the way redact.URL replaces it inside a URL.
		return redactedUserinfo
	case fmt.Stringer:
		// The handler would render it through String() anyway; redact
		// that rendering rather than let it reach the log unseen.
		return s.redaction.Text(t.String())
	}
	return s.resolveReflected(reflect.ValueOf(v), v, depth)
}

// redactedUserinfo replaces a url.Userinfo value; it is the placeholder
// redact.URL puts in place of a URL's userinfo, so a Userinfo logged on its
// own and one inside a URL read the same.
const redactedUserinfo = "redacted"

// resolveReflected walks v by kind so a container of any static type is
// covered: a string (named string types included) is redacted, a map is
// rebuilt as map[string]any with every key and value resolved (a key that
// is not a string is rendered from its resolved form, so a map[int]string
// cannot carry a value past the rules) and a slice or array as []any with
// every element resolved, a struct is rebuilt as a
// map[string]any of its exported fields (each resolved; one with no
// exported field is replaced by a placeholder naming its type, since its
// unexported fields cannot be inspected and must not be printed), a
// pointer to one of those is followed, and a []byte or anything else
// (numbers, bools) is returned as is. orig is the value v was taken from,
// returned unchanged for the kinds that are left alone.
func (s *logrSink) resolveReflected(rv reflect.Value, orig any, depth int) any {
	switch rv.Kind() {
	case reflect.String:
		return s.redaction.Text(rv.String())
	case reflect.Struct:
		rt := rv.Type()
		out := make(map[string]any, rt.NumField())
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			out[f.Name] = s.resolveDepth(rv.Field(i).Interface(), depth+1)
		}
		if len(out) == 0 {
			// Nothing the walk can inspect; rendering the unexported fields
			// with %v would print whatever they hold, so name the type and
			// omit the value.
			return fmt.Sprintf("[omitted: opaque %T]", orig)
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return orig
		}
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			var key string
			if it.Key().Kind() == reflect.String {
				key = s.redaction.Text(it.Key().String())
			} else {
				key = fmt.Sprint(s.resolveDepth(it.Key().Interface(), depth+1))
			}
			out[key] = s.resolveDepth(it.Value().Interface(), depth+1)
		}
		return out
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 || (rv.Kind() == reflect.Slice && rv.IsNil()) {
			return orig
		}
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = s.resolveDepth(rv.Index(i).Interface(), depth+1)
		}
		return out
	case reflect.Pointer:
		if rv.IsNil() {
			return orig
		}
		switch rv.Elem().Kind() {
		case reflect.String, reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
			return s.resolveDepth(rv.Elem().Interface(), depth+1)
		default:
			return orig
		}
	default:
		return orig
	}
}

// resolveAttr redacts a string-valued attribute and rebuilds a group
// attribute member by member, so an endpoint nested inside slog.Group is
// redacted like one at the top level. A LogValuer is resolved first so its
// output is what gets inspected, and any other kind is unwrapped and
// resolved as a plain value, which covers a map or slice carried as
// slog.Any. The depth bound applies to groups too: a group nested deeper
// than maxResolveDepth is replaced by tooDeep rather than descended into
// or passed through.
func (s *logrSink) resolveAttr(a slog.Attr, depth int) slog.Attr {
	if depth > maxResolveDepth {
		return slog.String(a.Key, tooDeep)
	}
	val := a.Value.Resolve()
	switch val.Kind() {
	case slog.KindString:
		return slog.String(a.Key, s.redaction.Text(val.String()))
	case slog.KindGroup:
		members := val.Group()
		args := make([]any, 0, len(members))
		for _, m := range members {
			args = append(args, s.resolveAttr(m, depth+1))
		}
		return slog.Group(a.Key, args...)
	default:
		return slog.Any(a.Key, s.resolveDepth(val.Any(), depth+1))
	}
}

// WithValues implements logr.LogSink.
func (s *logrSink) WithValues(keysAndValues ...any) logr.LogSink {
	clone := *s
	clone.values = append(append([]any(nil), s.values...), keysAndValues...)
	return &clone
}

// WithName implements logr.LogSink.
func (s *logrSink) WithName(name string) logr.LogSink {
	clone := *s
	clone.name = strings.TrimPrefix(s.name+"/"+name, "/")
	return &clone
}
