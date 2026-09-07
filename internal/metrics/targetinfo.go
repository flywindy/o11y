package metrics

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/sdk/resource"
)

// targetInfoName and targetInfoHelp match what otelprom emits, so nothing a
// dashboard joins on changes when the SDK renders the family itself.
const (
	targetInfoName = "target_info"
	targetInfoHelp = "Target metadata"
)

// targetInfoCollector renders target_info from the Resource the SDK built
// instead of the one the MeterProvider holds.
//
// The OTel SDK merges resource.Environment() into every provider's Resource
// unconditionally (sdk/metric config.go WithResource, sdk/trace
// provider.go), so the guard EnvResourceAttributes applies while the
// Resource is built never reaches the exporter's own target_info: an
// environment alias such as telemetry_sdk_name=evil comes back through that
// merge and otelprom joins it into the detected label
// ("evil;opentelemetry"). Rendering the family here, from the guarded
// Resource, closes the gap; the exporter's copy is disabled with
// otelprom.WithoutTargetInfo. Constant labels are unaffected either way: the
// allow filter matches exact keys, and an alias is a different key.
type targetInfoCollector struct {
	desc   *prometheus.Desc
	metric prometheus.Metric
}

// newTargetInfoCollector builds the collector for res. Label names follow
// the same normalization otelprom applies; a key whose label client_golang
// would reject ("__meta__", punctuation-only) is skipped, and when two keys
// still render as the same label the first in key order wins instead of
// being joined.
func newTargetInfoCollector(res *resource.Resource) (*targetInfoCollector, error) {
	keys, values := targetInfoLabels(res)
	desc := prometheus.NewDesc(targetInfoName, targetInfoHelp, keys, nil)
	metric, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, 1, values...)
	if err != nil {
		return nil, fmt.Errorf("metrics: build target_info: %w", err)
	}
	return &targetInfoCollector{desc: desc, metric: metric}, nil
}

// targetInfoLabels translates the resource attributes into Prometheus label
// names and values, in key order.
func targetInfoLabels(res *resource.Resource) (keys, values []string) {
	if res == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	iter := res.Set().Iter()
	for iter.Next() {
		kv := iter.Attribute()
		label := NormalizePrometheusLabelName(string(kv.Key))
		if label == "" || strings.HasPrefix(label, "__") || !hasAlphanumeric(string(kv.Key)) {
			continue // client_golang rejects the first two; the translator rejects the third
		}
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		keys = append(keys, label)
		values = append(values, kv.Value.String())
	}
	return keys, values
}

// Describe implements prometheus.Collector.
func (c *targetInfoCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *targetInfoCollector) Collect(ch chan<- prometheus.Metric) { ch <- c.metric }
