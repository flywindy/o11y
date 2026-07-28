package nats

import natsgo "github.com/nats-io/nats.go"

// ConnectOption configures ConnectWithOptions.
type ConnectOption func(*connectConfig)

type connectConfig struct {
	tracingEnabled bool
	natsOpts       []natsgo.Option
}

func newConnectConfig(opts []ConnectOption) connectConfig {
	cfg := connectConfig{tracingEnabled: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// WithTracingEnabled controls whether the underlying otel-nats connection uses
// the traced wrapper path.
//
// The default is true, matching Connect. Set false when a service-level
// observability toggle must preserve the native NATS cost profile: no spans, no
// propagation injection/extraction, and JetStream wrappers backed by the
// upstream direct implementation.
func WithTracingEnabled(enabled bool) ConnectOption {
	return func(cfg *connectConfig) {
		cfg.tracingEnabled = enabled
	}
}

// WithNATSOptions forwards options to the underlying nats.go connection.
func WithNATSOptions(opts ...natsgo.Option) ConnectOption {
	return func(cfg *connectConfig) {
		cfg.natsOpts = append(cfg.natsOpts, opts...)
	}
}
