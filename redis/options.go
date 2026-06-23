package redis

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// Option configures Redis instrumentation behavior.
type Option func(*config)

type config struct {
	commandTextEnabled bool
	requireParentSpan  bool
	attrs              []attribute.KeyValue
	ignored            map[string]struct{}
	poolName           string
}

// WithCommandTextEnabled controls whether the full Redis command text is
// recorded as db.query.text on spans.
//
// The default is false because Redis commands commonly contain sensitive key
// names or values. When enabled, command text is truncated to 1 KiB.
func WithCommandTextEnabled(enabled bool) Option {
	return func(cfg *config) {
		cfg.commandTextEnabled = enabled
	}
}

// WithAttributes appends extra attributes to every emitted Redis span.
//
// The SDK's built-in semantic-convention attributes (db.system.name,
// db.operation.name, server.address, server.port, db.namespace, db.query.text)
// take precedence: if a supplied attribute reuses one of those keys, the
// built-in value wins and the supplied one is dropped.
//
// These attributes are never applied to metric samples; Redis metric labels are
// fixed by the SDK to keep cardinality bounded.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(cfg *config) {
		cfg.attrs = append(cfg.attrs, attrs...)
	}
}

// WithIgnoredCommands suppresses both the span and the
// db.client.operation.duration sample for the named Redis commands. Names are
// matched case-insensitively against the command verb (cmd.Name(), e.g. "ping",
// "info", "echo", "cluster"), so a single entry covers every invocation of that
// command regardless of its arguments.
//
// Use it to drop high-volume, low-value commands an application issues outside
// of its request path — most commonly health-check / keepalive PINGs and
// periodic INFO polls. These are real db.* operations (unlike Pub/Sub and
// connection-lifecycle commands, which the wrapper always filters), so the SDK
// emits them by default and leaves the decision to suppress them to the caller.
//
// Filtering is by command name only and therefore cannot distinguish a
// background health-check PING from an application-issued PING — WithIgnoredCommands("ping")
// drops both. When only background probes are noisy, prefer either
// WithRequireParentSpan or a dedicated unwrapped client for the probe.
func WithIgnoredCommands(names ...string) Option {
	return func(cfg *config) {
		for _, name := range names {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if cfg.ignored == nil {
				cfg.ignored = make(map[string]struct{})
			}
			cfg.ignored[name] = struct{}{}
		}
	}
}

// WithRequireParentSpan restricts instrumentation to commands issued while a
// valid parent span is active on the context. When enabled, a command (or
// pipeline) whose context carries no valid span context produces neither a span
// nor a db.client.operation.duration sample.
//
// This is the cleanest filter for background noise that command-name matching
// cannot target precisely: health-check probes, pool keepalive, and periodic
// topology refreshes typically run on a fresh context with no request span,
// while genuine application commands run within a server/consumer span and are
// kept. The trade-off is that legitimate background work (scheduled jobs, warmup
// tasks) issued without a parent span is also dropped, so this is opt-in and off
// by default.
func WithRequireParentSpan(enabled bool) Option {
	return func(cfg *config) {
		cfg.requireParentSpan = enabled
	}
}

// WithPoolName sets the application-defined connection-pool name used on Redis
// pool metric samples.
//
// When omitted, Wrap synthesizes a process-unique name from the wrapped client
// pointer. Cluster and Ring clients append each shard address to the base name.
func WithPoolName(name string) Option {
	return func(cfg *config) {
		cfg.poolName = name
	}
}

func newConfig(opts []Option) config {
	cfg := config{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}
