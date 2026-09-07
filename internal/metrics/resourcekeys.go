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

// IdentityResourceKeys are the resource attributes the SDK's identity options
// own (WithServiceName / WithServiceVersion / WithServiceNamespace /
// WithEnvironment). They are always set last when the Resource is built, so
// nothing else can override them by the exact key; the guards below make sure
// nothing shadows them through an alias either.
var IdentityResourceKeys = []attribute.Key{
	semconv.ServiceNameKey,
	semconv.ServiceVersionKey,
	semconv.ServiceNamespaceKey,
	semconv.DeploymentEnvironmentNameKey,
}

// DetectedResourceKeys are the resource attributes the SDK's own detectors set
// (resource.WithTelemetrySDK, the narrow process detectors, resource.WithHost).
// Listed so a caller or the environment cannot shadow one, by the exact key or
// by an alias that renders as the same Prometheus label.
var DetectedResourceKeys = []attribute.Key{
	semconv.TelemetrySDKNameKey,
	semconv.TelemetrySDKLanguageKey,
	semconv.TelemetrySDKVersionKey,
	semconv.ProcessPIDKey,
	semconv.ProcessExecutableNameKey,
	semconv.ProcessRuntimeNameKey,
	semconv.ProcessRuntimeVersionKey,
	semconv.HostNameKey,
}

// telemetrySDKLabelPrefix is the Prometheus form of the telemetry.sdk.*
// namespace, which identifies the OpenTelemetry SDK rather than the service.
const telemetrySDKLabelPrefix = "telemetry_sdk_"

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
	for _, k := range IdentityResourceKeys {
		if SameLabel(key, k) {
			return k, true
		}
	}
	for _, k := range DetectedResourceKeys {
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
//   - an alias of an SDK-owned key (IdentityResourceKeys,
//     DetectedResourceKeys): "telemetry_sdk_name" next to the detected
//     telemetry.sdk.name. The exact SDK-owned key is kept: the SDK sets it
//     again later in the merge, so it is overridden cleanly, and
//     OTEL_SERVICE_NAME is the documented way to seed service.name;
//   - any other key in the telemetry.sdk.* namespace;
//   - a key the Prometheus exporter reserves or cannot translate
//     (IsReservedAttributeKey);
//   - an alias of a key in callerAttrs, which WithResourceAttributes already
//     set: the option is documented to win over the environment, and an alias
//     would otherwise sit next to it instead of being overridden;
//   - an alias of an environment key kept earlier (keys are visited in
//     sorted order, so the choice is deterministic).
//
// The OTel providers merge resource.Environment() back into the Resource
// they are given, so the provider-side Resource still carries a dropped
// alias. The metrics paths correct for that where the Resource is
// exported: target_info is rendered by targetInfoCollector from the guarded
// Resource on the Prometheus pull path, and guardedResourceExporter ships
// the guarded Resource on the OTLP push path, so no Prometheus translation
// on either path sees the alias. Spans and log records still carry it as
// its own, distinct attribute: those providers offer no seam short of
// mutating the process environment, and no Prometheus label is derived
// from them; the startup warning names the key to remove.
func EnvResourceAttributes(ctx context.Context, callerAttrs []attribute.KeyValue) (kept []attribute.KeyValue, warnings []string) {
	envRes, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, []string{fmt.Sprintf("OTEL_RESOURCE_ATTRIBUTES: ignored: %v", err)}
	}
	if envRes == nil {
		return nil, nil
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
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, which the SDK sets itself",
				string(kv.Key), string(owner)))
		case owner == "" && IsTelemetrySDKKey(kv.Key):
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; telemetry.sdk.* identifies the OpenTelemetry SDK, not the service",
				string(kv.Key)))
		case IsReservedAttributeKey(kv.Key):
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; as the Prometheus label %q it is reserved by the exporter or not a valid label name, and otelprom would disable target_info for the whole process",
				string(kv.Key), NormalizePrometheusLabelName(string(kv.Key))))
		case aliasOf(kv.Key, callerAttrs) != "":
			warnings = append(warnings, fmt.Sprintf(
				"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, which WithResourceAttributes already sets",
				string(kv.Key), string(aliasOf(kv.Key, callerAttrs))))
		default:
			if prev, dup := seen[label]; dup {
				warnings = append(warnings, fmt.Sprintf(
					"OTEL_RESOURCE_ATTRIBUTES: ignoring %q; it would render as the same Prometheus label as %q, given earlier in OTEL_RESOURCE_ATTRIBUTES",
					string(kv.Key), string(prev)))
				continue
			}
			seen[label] = kv.Key
			kept = append(kept, kv)
		}
	}
	return kept, warnings
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
