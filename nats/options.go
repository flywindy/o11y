package nats

import natsgo "github.com/nats-io/nats.go"

// ConnectOption configures ConnectWithOptions.
type ConnectOption func(*connectConfig)

type connectConfig struct {
	tracingDefault bool
	natsOpts       []natsgo.Option
}

func newConnectConfig(opts []ConnectOption) connectConfig {
	cfg := connectConfig{tracingDefault: true}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// WithTracingEnabled supplies the connection-local default for the underlying
// otel-nats tracing switch.
//
// The default is true, matching Connect. Since otel-nats v0.8.0 this option is
// the third rung of relay > environment > option > module default. An
// OTEL_NATS_TRACING_ENABLED value or a configured feature-flag relay can
// therefore override it in either direction. When neither is configured,
// false still selects the upstream direct implementation with no spans, no
// propagation injection/extraction, and no per-operation flag evaluation.
func WithTracingEnabled(enabled bool) ConnectOption {
	return func(cfg *connectConfig) {
		cfg.tracingDefault = enabled
	}
}

// WithNATSOptions forwards options to the underlying nats.go connection.
func WithNATSOptions(opts ...natsgo.Option) ConnectOption {
	return func(cfg *connectConfig) {
		cfg.natsOpts = append(cfg.natsOpts, opts...)
	}
}
