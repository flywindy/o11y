package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
)

// identityResourceKeys are the resource attributes the SDK's identity options
// own (WithServiceName / WithServiceVersion / WithServiceNamespace /
// WithEnvironment). They are always set last when the Resource is built, so
// nothing else can override them by the exact key; the guards below make sure
// nothing shadows them through an alias either. Both tables are package
// private and never written after initialization, so no caller can change
// the ownership policy or race with it.
var identityResourceKeys = []attribute.Key{
	semconv.ServiceNameKey,
	semconv.ServiceVersionKey,
	semconv.ServiceNamespaceKey,
	semconv.DeploymentEnvironmentNameKey,
}

// detectedResourceKeys are the resource attributes the SDK's own detectors set
// (resource.WithTelemetrySDK, the narrow process detectors, resource.WithHost).
// Listed so a caller or the environment cannot shadow one, by the exact key or
// by an alias that renders as the same Prometheus label.
var detectedResourceKeys = []attribute.Key{
	semconv.TelemetrySDKNameKey,
	semconv.TelemetrySDKLanguageKey,
	semconv.TelemetrySDKVersionKey,
	semconv.ProcessPIDKey,
	semconv.ProcessExecutableNameKey,
	semconv.ProcessRuntimeNameKey,
	semconv.ProcessRuntimeVersionKey,
	semconv.HostNameKey,
}

// telemetrySDKLabelPrefix and processLabelPrefix are the Prometheus forms of
// the two namespaces the SDK reserves as a whole: telemetry.sdk.* identifies
// the OpenTelemetry SDK rather than the service, and process.* is what the
// SDK's own detectors describe — process.command_args and process.owner are
// left out of them on purpose (see o11y.buildResource), so nothing may add a
// process.* attribute back from the outside.
const (
	telemetrySDKLabelPrefix = "telemetry_sdk_"
	processLabelPrefix      = "process_"
)

// SameLabel reports whether two attribute keys render as the same Prometheus
// label. That is where resource-attribute collisions happen: otelprom joins
// the values of two attributes that normalize alike into one target_info
// label ("evil;svc"), so "service_name" or "service-name" must be treated
// exactly like "service.name".
func SameLabel(a, b attribute.Key) bool {
	return NormalizePrometheusLabelName(string(a)) == NormalizePrometheusLabelName(string(b))
}

// ResourceKeyOwner returns the SDK-owned resource key that key would render
// as the same Prometheus label as, and whether that owner is one of the
// identity keys. It returns "" when no SDK-owned key matches.
func ResourceKeyOwner(key attribute.Key) (owner attribute.Key, identity bool) {
	for _, k := range identityResourceKeys {
		if SameLabel(key, k) {
			return k, true
		}
	}
	for _, k := range detectedResourceKeys {
		if SameLabel(key, k) {
			return k, false
		}
	}
	return "", false
}

// IsTelemetrySDKKey reports whether key falls in the telemetry.sdk.*
// namespace once rendered as a Prometheus label.
func IsTelemetrySDKKey(key attribute.Key) bool {
	return strings.HasPrefix(NormalizePrometheusLabelName(string(key)), telemetrySDKLabelPrefix)
}

// IsProcessKey reports whether key falls in the process.* namespace once
// rendered as a Prometheus label. The SDK collects the process attributes it
// is willing to export itself and deliberately leaves process.command_args
// and process.owner out, so the whole namespace is closed to callers and the
// environment.
func IsProcessKey(key attribute.Key) bool {
	return strings.HasPrefix(NormalizePrometheusLabelName(string(key)), processLabelPrefix)
}

// EnvResourceAttributes reads OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME
// the way resource.WithFromEnv does and returns the attributes that may go
// into the Resource, with one warning per attribute it drops. It is used in
// place of resource.WithFromEnv so the environment goes through the same
// guard as WithResourceAttributes; the environment is the one input the
// option cannot validate at option time.
//
// Dropped, because otelprom would join the two values into one target_info
// label or refuse the label outright:
//
//   - an alias of an SDK-owned key (identityResourceKeys,
//     detectedResourceKeys): "telemetry_sdk_name" next to the detected
//     telemetry.sdk.name. The exact SDK-owned key is kept: the SDK sets it
//     again later in the merge, so it is overridden cleanly, and
//     OTEL_SERVICE_NAME is the documented way to seed service.name;
//   - any other key in the telemetry.sdk.* or process.* namespaces: the
//     latter would let process.command_args back in through the environment;
//   - a key the Prometheus exporter reserves or cannot translate
//     (IsReservedAttributeKey);
//   - an alias of a key in callerAttrs, which WithResourceAttributes already
//     set: the option is documented to win over the environment, and an alias
//     would otherwise sit next to it instead of being overridden;
//   - an alias of an environment key kept earlier (keys are visited in
//     sorted order, so the choice is deterministic).
//
// The OTel providers merge resource.Environment() back underneath the
// Resource they are given, so a dropped key would come back on the provider
// side with its environment value. The metrics paths export the guarded
// Resource directly (targetInfoCollector on the Prometheus pull path,
// guardedResourceExporter on the OTLP push path). The trace and log
// providers offer no such seam, so o11y.buildResource hands them the
// guarded Resource plus NeutralizeEnvKeys(dropped): the same keys with an
// empty value, which win the merge by exact key. A span or log record from
// a misconfigured environment therefore carries process_command_args=""
// rather than the command line; the warning names the key to remove.
func EnvResourceAttributes(ctx context.Context, callerAttrs []attribute.KeyValue) (kept []attribute.KeyValue, dropped []attribute.Key, warnings []string) {
	envRes, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, nil, []string{fmt.Sprintf("OTEL_RESOURCE_ATTRIBUTES: ignored: %v", err)}
	}
	if envRes == nil {
		return nil, nil, nil
	}
	seen := make(map[string]attribute.Key)   // normalized label → env key already kept
	for _, kv := range envRes.Attributes() { // sorted by key
		owner, _ := ResourceKeyOwner(kv.Key)
		label := NormalizePrometheusLabelName(string(kv.Key))
		switch {
		case owner == kv.Key:
			// The exact SDK-owned key: the SDK sets it again later in the
			// merge, so it is overridden cleanly. OTEL_SERVICE_NAME lands here.
			seen[label] = kv.Key
			kept = append(kept, kv)
		case owner != "":
			dropped = append(dropped, kv.Key)
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, which the SDK sets itself",
				string(kv.Key), string(owner)))
		case owner == "" && IsTelemetrySDKKey(kv.Key):
			dropped = append(dropped, kv.Key)
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; telemetry.sdk.* identifies the OpenTelemetry SDK, not the service",
				string(kv.Key)))
		case owner == "" && IsProcessKey(kv.Key):
			dropped = append(dropped, kv.Key)
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; process.* is collected by the SDK itself, and process.command_args / process.owner are deliberately not exported",
				string(kv.Key)))
		case IsReservedAttributeKey(kv.Key):
			dropped = append(dropped, kv.Key)
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; as the Prometheus label %q it is reserved by the exporter or not a valid label name, and otelprom would disable target_info for the whole process",
				string(kv.Key), NormalizePrometheusLabelName(string(kv.Key))))
		case aliasOf(kv.Key, callerAttrs) != "":
			dropped = append(dropped, kv.Key)
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, which WithResourceAttributes already sets",
				string(kv.Key), string(aliasOf(kv.Key, callerAttrs))))
		default:
			if prev, dup := seen[label]; dup {
				dropped = append(dropped, kv.Key)
				warnings = append(warnings, fmt.Sprintf(
					"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, given earlier in OTEL_RESOURCE_ATTRIBUTES",
					string(kv.Key), string(prev)))
				continue
			}
			seen[label] = kv.Key
			kept = append(kept, kv)
		}
	}
	return kept, dropped, warnings
}

// NeutralizeEnvKeys returns dropped as attributes with an empty string value.
// Merged on top of the guarded Resource before it is handed to a provider,
// they override the environment's values by exact key when the provider
// merges resource.Environment() back in, without touching the process
// environment itself. The environment parses every value as a string, so
// the type matches.
func NeutralizeEnvKeys(dropped []attribute.Key) []attribute.KeyValue {
	if len(dropped) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(dropped))
	for _, k := range dropped {
		out = append(out, attribute.String(string(k), ""))
	}
	return out
}

// aliasOf returns the key in attrs that key renders as the same Prometheus
// label as while being a different key, or "" when there is none. The same
// exact key is not an alias: a later value for it overrides the earlier one.
func aliasOf(key attribute.Key, attrs []attribute.KeyValue) attribute.Key {
	for _, kv := range attrs {
		if kv.Key != key && SameLabel(kv.Key, key) {
			return kv.Key
		}
	}
	return ""
}
