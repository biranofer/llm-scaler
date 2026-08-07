package config

import (
	"os"
	"sort"

	"gopkg.in/yaml.v3"
	ctrl "sigs.k8s.io/controller-runtime"
)

// DefaultAnalyzerCatalogConfigMapName is the default name of the external-analyzer
// catalog ConfigMap.
const DefaultAnalyzerCatalogConfigMapName = "wva-analyzers"

// AnalyzerCatalogConfigMapName returns the external-analyzer catalog ConfigMap
// name, honoring the ANALYZER_CATALOG_CONFIG_MAP_NAME override.
func AnalyzerCatalogConfigMapName() string {
	if name := os.Getenv("ANALYZER_CATALOG_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultAnalyzerCatalogConfigMapName
}

// ExternalAnalyzerBody is one inference engine's query and per-replica target in
// a catalog definition. The field names mirror KEDA's Prometheus scaler
// (threshold is a string, e.g. "0.5").
//
// query must resolve to the model's TOTAL demand (every series it returns is
// summed) and threshold is what ONE scale-target replica — a pod, or an LWS
// group — can serve, not one engine instance. A sum-shaped query is safe at any
// granularity; count() and avg() are not, since they measure in DP ranks. See
// external.Body for the full note.
type ExternalAnalyzerBody struct {
	Query     string `yaml:"query"`
	Threshold string `yaml:"threshold"`
}

// ExternalAnalyzerDef is a catalog entry for one external analyzer. It carries a
// per-engine body keyed by engine ("vllm", "sglang"), or an engine-agnostic body
// given by the top-level Query/Threshold when Engines is empty.
type ExternalAnalyzerDef struct {
	Engines   map[string]ExternalAnalyzerBody `yaml:"engines,omitempty"`
	Query     string                          `yaml:"query,omitempty"`
	Threshold string                          `yaml:"threshold,omitempty"`
}

// ExternalAnalyzerCatalog maps an analyzer label to its definition.
type ExternalAnalyzerCatalog map[string]ExternalAnalyzerDef

// ParseAnalyzerCatalogConfigMap parses the external-analyzer catalog ConfigMap.
// Each data key is an analyzer label whose value is the YAML ExternalAnalyzerDef.
// A malformed entry is skipped (logged) so one bad definition cannot break the
// whole catalog. Keys are processed in sorted order for deterministic logging.
func ParseAnalyzerCatalogConfigMap(data map[string]string) ExternalAnalyzerCatalog {
	out := make(ExternalAnalyzerCatalog)
	if data == nil {
		return out
	}
	labels := make([]string, 0, len(data))
	for k := range data {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, label := range labels {
		var def ExternalAnalyzerDef
		if err := yaml.Unmarshal([]byte(data[label]), &def); err != nil {
			ctrl.Log.Info("Failed to parse analyzer catalog entry, skipping", "label", label, "error", err)
			continue
		}
		out[label] = def
	}
	return out
}

// catalogConfig holds the parsed external-analyzer catalog. Unlike the saturation
// and scale-to-zero configs it is cluster-scoped (not namespace-aware) — the
// catalog defines analyzers for the whole cluster; policies select them per tier.
type catalogConfig struct {
	catalog ExternalAnalyzerCatalog
}

// ExternalAnalyzerCatalog returns a copy of the current external-analyzer catalog.
func (c *Config) ExternalAnalyzerCatalog() ExternalAnalyzerCatalog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copyAnalyzerCatalog(c.catalog.catalog)
}

// UpdateExternalAnalyzerCatalog replaces the external-analyzer catalog.
func (c *Config) UpdateExternalAnalyzerCatalog(cat ExternalAnalyzerCatalog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.catalog.catalog = copyAnalyzerCatalog(cat)
}

// copyAnalyzerCatalog deep-copies a catalog (including each def's Engines map) so
// stored and returned catalogs cannot be mutated through a shared reference.
func copyAnalyzerCatalog(src ExternalAnalyzerCatalog) ExternalAnalyzerCatalog {
	out := make(ExternalAnalyzerCatalog, len(src))
	for label, def := range src {
		cp := ExternalAnalyzerDef{Query: def.Query, Threshold: def.Threshold}
		if def.Engines != nil {
			cp.Engines = make(map[string]ExternalAnalyzerBody, len(def.Engines))
			for engine, body := range def.Engines {
				cp.Engines[engine] = body
			}
		}
		out[label] = cp
	}
	return out
}
