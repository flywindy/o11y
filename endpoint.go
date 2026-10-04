package o11y

import (
	"fmt"
	"net/url"
	"strings"
)

// The standard variables that name an OTLP endpoint. A signal variable is
// one signal's endpoint; the generic one is a base URL each signal's path
// (/v1/traces, /v1/logs) is appended to.
const (
	envOTLPEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envOTLPLogsEndpoint   = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	envOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
)

// defaultOTLPEndpoint is where traces and logs go when neither
// WithOTLPEndpoint nor an endpoint variable says otherwise.
const defaultOTLPEndpoint = "http://localhost:4318"

// otlpSignal names what endpoint resolution needs to know about one signal
// sent to the WithOTLPEndpoint collector.
type otlpSignal struct {
	name        string // "trace" or "log", for errors
	envVar      string // the signal's own endpoint variable
	protocolVar string // the signal's own protocol variable
	urlPath     string // the OTLP path appended to the generic variable
}

var (
	traceSignal = otlpSignal{name: "trace", envVar: envOTLPTracesEndpoint, protocolVar: "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", urlPath: "/v1/traces"}
	logSignal   = otlpSignal{name: "log", envVar: envOTLPLogsEndpoint, protocolVar: "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", urlPath: "/v1/logs"}
)

// signalEndpoint is where one signal's exporter sends and what said so.
type signalEndpoint struct {
	// url is what Init passes to the exporter. A WithOTLPEndpoint value (or
	// the default) is passed as given, and the exporter setup adds the
	// signal's default path as it always has; a URL from a variable is
	// passed complete, path included, so that setup leaves it alone.
	url string
	// source is "WithOTLPEndpoint", the variable the endpoint came from, or
	// "" for the SDK's default.
	source string
	// raw is the value the endpoint came from.
	raw string
	// problem is why raw cannot be used (see endpointProblem), or "".
	problem string
	// grpcVar names the protocol variable that asks for gRPC when the
	// endpoint came from a variable, or is "" (see resolveEndpoint).
	grpcVar string
}

// fromEnv reports whether the endpoint came from a variable.
func (e signalEndpoint) fromEnv() bool {
	return e.source != "" && e.source != "WithOTLPEndpoint"
}

// otlpEndpoints holds the trace and log endpoints, resolved once by Init
// (see resolveOTLPEndpoints) so the endpoint it validates is the one the
// exporters get and the diagnostics redact.
type otlpEndpoints struct {
	trace, log signalEndpoint
}

// resolveOTLPEndpoints resolves both signals' endpoints into cfg.
func resolveOTLPEndpoints(cfg *Config) {
	cfg.resolvedOTLP = otlpEndpoints{
		trace: resolveEndpoint(cfg, traceSignal),
		log:   resolveEndpoint(cfg, logSignal),
	}
}

// resolveEndpoint returns the endpoint for one signal. Init resolves it
// itself rather than leaving the variables to the exporters, so traces and
// logs read them alike: trimmed, with one path rule, and with TLS decided
// by the URL's scheme (an explicit endpoint overrides
// OTEL_EXPORTER_OTLP_INSECURE in both exporters). The precedence is the
// OTel specification's: WithOTLPEndpoint, then the signal variable, then
// the generic one, then the default (http://localhost:4318).
//
// A signal variable is used as given, minus any trailing slash, and one
// with no path gets the signal's default path where the specification
// sends to "/". The generic variable is a base URL: the signal's path is
// appended to its path, a trailing slash dropped.
//
// An endpoint from a variable is often injected next to a protocol
// variable; when that asks for gRPC (the signal's PROTOCOL variable, else
// the generic one), the endpoint is a gRPC port this SDK cannot send its
// OTLP/HTTP requests to, so grpcVar records it for Init to reject.
func resolveEndpoint(cfg *Config, s otlpSignal) signalEndpoint {
	if cfg.otlpEndpointSet {
		return newSignalEndpoint(cfg.otlpEndpoint, "WithOTLPEndpoint")
	}
	var e signalEndpoint
	if v := envValue(s.envVar, false); v != "" {
		e = newEnvSignalEndpoint(v, s.envVar, s.urlPath, false)
	} else if v := envValue(envOTLPEndpoint, false); v != "" {
		e = newEnvSignalEndpoint(v, envOTLPEndpoint, s.urlPath, true)
	} else {
		return newSignalEndpoint(cfg.otlpEndpoint, "")
	}
	protocolVar := s.protocolVar
	if envValue(protocolVar, false) == "" {
		protocolVar = envOTLPProtocol
	}
	if strings.EqualFold(envValue(protocolVar, false), "grpc") {
		e.grpcVar = protocolVar
	}
	return e
}

func newSignalEndpoint(v, source string) signalEndpoint {
	_, problem := parseEndpoint(v)
	return signalEndpoint{url: v, source: source, raw: v, problem: problem}
}

// newEnvSignalEndpoint completes a variable's URL with the signal's path:
// appended to a base URL's path, or standing in for a signal URL's missing
// one. It edits the decoded path, the one the exporters send to: they keep
// only a URL's host and path, as they do for WithOTLPEndpoint.
func newEnvSignalEndpoint(v, source, urlPath string, base bool) signalEndpoint {
	e := signalEndpoint{url: v, source: source, raw: v}
	u, problem := parseEndpoint(v)
	if problem != "" {
		e.problem = problem
		return e
	}
	p := strings.TrimRight(u.Path, "/")
	if base || p == "" {
		p += urlPath
	}
	u.Path, u.RawPath = p, ""
	e.url = u.String()
	return e
}

// The reasons endpointProblem gives. None quotes the value, whose userinfo
// may hold a credential.
const (
	problemNoScheme   = "it has no http:// or https:// scheme"
	problemInvalidURL = "it is not a valid URL"
	problemNoHost     = "it has no host"
)

// endpointProblem reports why v cannot serve as an OTLP/HTTP endpoint, or ""
// when it can: it must be a URL with an http or https scheme and a host. A
// scheme-less "collector:4318" parses with "collector" as its scheme and no
// host, "127.0.0.1:4318" does not parse at all, and "http:/collector" has
// no host; each would leave an exporter that never sends.
func endpointProblem(v string) string {
	_, problem := parseEndpoint(v)
	return problem
}

// parseEndpoint is endpointProblem returning the parsed URL as well, nil
// when there is a problem.
// A host of only a port ("http://:4318") counts as a host: the exporters
// dial it on the local machine.
func parseEndpoint(v string) (*url.URL, string) {
	u, err := url.Parse(v)
	switch {
	case err != nil && !strings.Contains(v, "://"):
		return nil, problemNoScheme // "10.0.0.7:4318" does not parse
	case err != nil:
		return nil, problemInvalidURL
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return nil, problemNoScheme
	}
	if u.Host == "" {
		return nil, problemNoHost
	}
	return u, ""
}

// endpointRule closes every endpoint error: what an endpoint must be, and
// that the value, which may hold a credential, is not quoted.
const endpointRule = "(an OTLP/HTTP endpoint is an http:// or https:// URL with a host, such as http://collector:4318); the value is not repeated here"

// endpointError is the Init error for an endpoint endpointProblem rejects.
// It names where the endpoint came from, never the value.
func endpointError(source, problem string) error {
	return fmt.Errorf("o11y: the endpoint given to %s cannot be used: %s %s", source, problem, endpointRule)
}

// grpcEndpointError is the Init error for an endpoint variable that a
// protocol variable says is a gRPC endpoint.
func grpcEndpointError(variable, protocolVar, signal string) error {
	return fmt.Errorf("o11y: %s cannot be used as the %s endpoint: %s asks for grpc, and this SDK sends OTLP over HTTP only, so the endpoint would refuse every export; point the variable at the collector's OTLP/HTTP port (usually 4318) with the protocol unset or http/protobuf, or pass WithOTLPEndpoint", variable, signal, protocolVar)
}

// envEndpointError is endpointError for an endpoint variable.
func envEndpointError(variable, signal, problem string) error {
	return fmt.Errorf("o11y: %s cannot be used as the %s endpoint: %s %s", variable, signal, problem, endpointRule)
}

// diagnosticEndpoints returns every endpoint an exporter may put in an error
// it reports, so the diagnostics redact their credentials: the metrics and
// profiling endpoints, and for each enabled signal the endpoint Init
// resolved for it (see resolveOTLPEndpoints) — the WithOTLPEndpoint value or
// the default, or the variable's value and the URL derived from it. An
// endpoint no exporter sends to is left out — a variable the option
// overrides, or the default once variables decide every enabled signal —
// since redacting it could only blank out text that names the endpoint in
// use.
func diagnosticEndpoints(cfg *Config) []string {
	out := []string{cfg.metricsOTLPEndpoint, cfg.profilingEndpoint}
	for _, s := range []struct {
		enabled bool
		e       signalEndpoint
	}{{cfg.traceEnabled, cfg.resolvedOTLP.trace}, {cfg.logEnabled, cfg.resolvedOTLP.log}} {
		if s.enabled && s.e.problem == "" {
			out = append(out, s.e.raw, s.e.url)
		}
	}
	return out
}
