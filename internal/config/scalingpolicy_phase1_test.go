package config

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

func p1BoolPtr(v bool) *bool { return &v }

// validBaseConfig returns a defaulted, valid ScalingPolicy to isolate
// the field under test in Validate cases.
func validBaseConfig() ScalingPolicy {
	c := ScalingPolicy{}
	c.ApplyDefaults()
	return c
}

var _ = Describe("ScalingPolicy Phase 1 schema", func() {

	Describe("AnalyzerScoreConfig.EffectiveType", func() {
		It("returns Type when set", func() {
			a := AnalyzerScoreConfig{Type: "throughput", Name: "ignored"}
			Expect(a.EffectiveType()).To(Equal("throughput"))
		})
		It("falls back to Name when Type is empty", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveType()).To(Equal("saturation"))
		})
	})

	Describe("AnalyzerScoreConfig.Normalize", func() {
		It("folds well-known parameters into typed fields", func() {
			a := AnalyzerScoreConfig{
				Type: "saturation",
				Parameters: map[string]any{
					"scaleUpThreshold":  0.95,
					"scaleDownBoundary": 0.60,
					"score":             2.0,
					"enabled":           false,
				},
			}
			Expect(a.Normalize()).To(Succeed())
			Expect(a.ScaleUpThreshold).NotTo(BeNil())
			Expect(*a.ScaleUpThreshold).To(Equal(0.95))
			Expect(a.ScaleDownBoundary).NotTo(BeNil())
			Expect(*a.ScaleDownBoundary).To(Equal(0.60))
			Expect(a.Score).To(Equal(2.0))
			Expect(a.Enabled).NotTo(BeNil())
			Expect(*a.Enabled).To(BeFalse())
		})

		It("coerces integer parameters to float", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"score": 3}}
			Expect(a.Normalize()).To(Succeed())
			Expect(a.Score).To(Equal(3.0))
		})

		It("does not override a typed field already set from a top-level key", func() {
			a := AnalyzerScoreConfig{
				Type:             "saturation",
				ScaleUpThreshold: float64Ptr(0.80),
				Parameters:       map[string]any{"scaleUpThreshold": 0.99},
			}
			Expect(a.Normalize()).To(Succeed())
			Expect(*a.ScaleUpThreshold).To(Equal(0.80))
		})

		It("tolerates unknown parameter keys", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"future": "value"}}
			Expect(a.Normalize()).To(Succeed())
		})

		It("errors on a wrongly-typed known parameter", func() {
			a := AnalyzerScoreConfig{Type: "saturation", Parameters: map[string]any{"enabled": "yes"}}
			Expect(a.Normalize()).To(HaveOccurred())
		})
	})

	Describe("Validate — analyzers", func() {
		It("accepts a known analyzer type", func() {
			c := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Type: "saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
		It("accepts the legacy name-only form", func() {
			c := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Name: "saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
		It("accepts an unrecognized analyzer type (extensible via RegisterAnalyzer; ignored at runtime)", func() {
			c := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Type: "epp-saturation"}}}
			c.ApplyDefaults()
			Expect(c.Validate()).To(Succeed())
		})
	})

	Describe("Validate — limiters", func() {
		It("accepts a gpu-inventory entry with no quota fields", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "gpu-inventory"}}
			Expect(c.Validate()).To(Succeed())
		})
		It("rejects a gpu-inventory entry carrying quota fields", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "gpu-inventory", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("rejects an unknown limiter type", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "bogus"}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("accepts a valid inline quota entry", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{
				Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 32},
			}}
			Expect(c.Validate()).To(Succeed())
		})
		It("rejects an inline quota entry missing a name (via reused quota validation)", func() {
			c := validBaseConfig()
			c.Limiters = []QuotaLimiterConfig{{Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 32}}}
			Expect(c.Validate()).To(HaveOccurred())
		})
		It("rejects two inline quota entries sharing a name (via reused quota validation)", func() {
			c := validBaseConfig()
			dup := QuotaLimiterConfig{Type: "quota", Name: "dup", Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8}}
			c.Limiters = []QuotaLimiterConfig{dup, dup}
			err := c.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("duplicate limiter name"))
		})
	})

	Describe("Merge", func() {
		It("carries ScaleToZero from the override but not Limiters (cluster-default-scope)", func() {
			base := ScalingPolicy{}
			override := ScalingPolicy{
				ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(false)},
				Limiters:    []QuotaLimiterConfig{{Type: "gpu-inventory"}},
			}
			base.Merge(override)
			Expect(base.ScaleToZero).NotTo(BeNil())
			Expect(*base.ScaleToZero.Enabled).To(BeFalse())
			Expect(base.Limiters).To(BeEmpty(), "per-model limiters are not merged; only the global default is read")
		})
		It("keeps base ScaleToZero when the override omits it", func() {
			base := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(true)}}
			base.Merge(ScalingPolicy{Priority: 2.0})
			Expect(base.ScaleToZero).NotTo(BeNil())
			Expect(*base.ScaleToZero.Enabled).To(BeTrue())
		})
	})

	Describe("v1 opt-out contract", func() {
		It("is V2 when an analyzers list is present", func() {
			c := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Type: "saturation"}}}
			Expect(c.IsV2()).To(BeTrue())
		})
		It("is V1 when the analyzers section is deleted", func() {
			c := ScalingPolicy{}
			Expect(c.IsV2()).To(BeFalse())
		})
	})
})

var _ = Describe("Phase 1 envelope — YAML round-trip", func() {
	It("parses the full envelope and folds parameters through the parse path", func() {
		const y = `
model_id: granite-premium
namespace: production
priority: 2.0
scaleToZero: { enabled: false }
analyzers:
  - type: saturation
    parameters: { scaleUpThreshold: 0.95 }
limiters:
  - type: gpu-inventory
  - type: quota
    name: cluster-h100
    scope: cluster
    quotas: { H100: 32 }
`
		var c ScalingPolicy
		Expect(yaml.Unmarshal([]byte(y), &c)).To(Succeed())
		// Same order the reconciler applies: Normalize -> ApplyDefaults -> Validate.
		Expect(c.Normalize()).To(Succeed())
		c.ApplyDefaults()
		Expect(c.Validate()).To(Succeed())

		Expect(c.IsV2()).To(BeTrue())
		Expect(c.Analyzers).To(HaveLen(1))
		Expect(c.Analyzers[0].EffectiveType()).To(Equal("saturation"))
		Expect(c.Analyzers[0].ScaleUpThreshold).NotTo(BeNil())
		Expect(*c.Analyzers[0].ScaleUpThreshold).To(Equal(0.95))
		Expect(c.ScaleToZero).NotTo(BeNil())
		Expect(*c.ScaleToZero.Enabled).To(BeFalse())
		Expect(c.Limiters).To(HaveLen(2))
	})
})

// The scaling entry is the ONLY per-model surface for scale-to-zero. A separate
// wva-model-scale-to-zero-config ConfigMap used to carry the same two settings,
// which left three places to look and a precedence rule to remember.
var _ = Describe("ResolveScaleToZeroEnabled / ResolveScaleToZeroRetention", func() {
	AfterEach(func() { _ = os.Unsetenv("WVA_SCALE_TO_ZERO") })

	It("uses the inline setting when present", func() {
		sat := &ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(true)}}
		Expect(ResolveScaleToZeroEnabled(sat)).To(BeTrue())

		sat.ScaleToZero.Enabled = p1BoolPtr(false)
		Expect(ResolveScaleToZeroEnabled(sat)).To(BeFalse())
	})

	It("falls back to the deployment flag when the entry says nothing", func() {
		Expect(os.Setenv("WVA_SCALE_TO_ZERO", "true")).To(Succeed())
		Expect(ResolveScaleToZeroEnabled(&ScalingPolicy{})).To(BeTrue())
		Expect(ResolveScaleToZeroEnabled(nil)).To(BeTrue())
	})

	It("lets an explicit false beat the deployment flag", func() {
		// The pointer exists for exactly this: "not set" and "set to false" must
		// not collapse, or a model could never opt out of a cluster-wide default.
		Expect(os.Setenv("WVA_SCALE_TO_ZERO", "true")).To(Succeed())
		sat := &ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{Enabled: p1BoolPtr(false)}}
		Expect(ResolveScaleToZeroEnabled(sat)).To(BeFalse())
	})

	It("resolves the retention period, defaulting when absent or unparseable", func() {
		sat := &ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: "45s"}}
		Expect(ResolveScaleToZeroRetention(sat)).To(Equal(45 * time.Second))

		Expect(ResolveScaleToZeroRetention(&ScalingPolicy{})).To(Equal(DefaultScaleToZeroRetentionPeriod))

		// A typo must not scale a model down the instant it goes idle.
		bad := &ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: "10 minutes"}}
		Expect(ResolveScaleToZeroRetention(bad)).To(Equal(DefaultScaleToZeroRetentionPeriod))
	})
})

// Both settings live in one envelope, so a per-model override that sets only one
// of them must not silently discard the other.
var _ = Describe("ScaleToZero merge", func() {
	It("merges the envelope field by field", func() {
		base := ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{
			Enabled: p1BoolPtr(true), RetentionPeriod: "10m",
		}}
		base.Merge(ScalingPolicy{ScaleToZero: &ScaleToZeroEnvelope{RetentionPeriod: "30s"}})

		Expect(base.ScaleToZero.RetentionPeriod).To(Equal("30s"))
		Expect(base.ScaleToZero.Enabled).NotTo(BeNil(), "enabled must survive a retention-only override")
		Expect(*base.ScaleToZero.Enabled).To(BeTrue())
	})
})

var _ = Describe("Config.EffectiveLimiterMode / EffectiveQuotaEntries", func() {
	It("selects quota when the default entry declares an inline quota limiter", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{{
				Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 32},
			}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
		entries := c.EffectiveQuotaEntries()
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name).To(Equal("cluster-h100"))
	})

	It("selects inventory when the default entry declares a gpu-inventory limiter", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory))
	})

	// "Zero or more" is the ScalingPolicy schema's own wording for this list, and
	// zero has to mean zero. An implicit inventory limiter here used to bound
	// scaling for an operator who declared nothing — invisible in the config, and
	// impossible to turn off — while enableLimiter separately decided whether the
	// optimizer honoured it. One list, one answer.
	It("selects no limiter at all when none is declared", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{"default": {}})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeNone))
		Expect(c.EffectiveQuotaEntries()).To(BeEmpty())
	})

	It("selects no limiter for an explicitly empty list", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeNone))
	})

	// A mixed list is not a chain. Quota wins and the physical limiter is never
	// built, so a policy that reads as "bounded by real GPUs as well" consults no
	// physical capacity at all. Bounding by min(physical, quota) is #1003; until
	// then the only defence is that the drop is reported rather than silent.
	Describe("a mixed limiters list", func() {
		mixed := func() *Config {
			c := &Config{}
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
				"default": {Limiters: []QuotaLimiterConfig{
					{Type: "gpu-inventory"},
					{Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
						ClusterQuotas: map[string]int{"H100": 32}},
				}},
			})
			return c
		}

		It("builds the quota limiter only", func() {
			Expect(mixed().EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
		})

		It("reports the physical limiter as not enforced", func() {
			Expect(mixed().UnenforcedLimiterTypes()).To(Equal([]string{"gpu-inventory"}))
		})

		It("reports nothing when every declared limiter is enforced", func() {
			c := &Config{}
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
				"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
			})
			Expect(c.UnenforcedLimiterTypes()).To(BeEmpty())

			c = &Config{}
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
				"default": {Limiters: []QuotaLimiterConfig{
					{Type: "quota", Name: "a", Scope: QuotaScopeCluster,
						ClusterQuotas: map[string]int{"H100": 1}},
					{Type: "quota", Name: "b", Scope: QuotaScopeCluster,
						ClusterQuotas: map[string]int{"A100": 1}},
				}},
			})
			Expect(c.UnenforcedLimiterTypes()).To(BeEmpty(),
				"two quota entries are both built — a CompositeLimiter — so neither is dropped")
		})

		It("de-duplicates and keeps declaration order", func() {
			p := ScalingPolicy{Limiters: []QuotaLimiterConfig{
				{Type: "inventory"},
				{Type: "gpu-inventory"},
				{Type: "inventory"},
				{Type: "quota", Name: "a", Scope: QuotaScopeCluster},
			}}
			Expect(p.UnenforcedLimiterTypes()).To(Equal([]string{"inventory", "gpu-inventory"}))
		})

		It("reports nothing for a policy with no limiters at all", func() {
			Expect(ScalingPolicy{}.UnenforcedLimiterTypes()).To(BeEmpty())
		})
	})

	It("ignores limiters declared on a non-default (per-model) entry", func() {
		c := &Config{}
		c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
			"default": {Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}},
			"some/model#ns": {Limiters: []QuotaLimiterConfig{{
				Type: "quota", Name: "sneaky", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H100": 999},
			}}},
		})
		Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory), "only the default entry selects the limiter")
		Expect(c.EffectiveQuotaEntries()).To(BeEmpty())
	})

	It("quota wins over a co-present gpu-inventory entry, regardless of order", func() {
		quota := QuotaLimiterConfig{
			Type: "quota", Name: "cluster-h100", Scope: QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 32},
		}
		inv := QuotaLimiterConfig{Type: "gpu-inventory"}
		for _, limiters := range [][]QuotaLimiterConfig{{inv, quota}, {quota, inv}} {
			c := &Config{}
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
				"default": {Limiters: limiters},
			})
			Expect(c.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
			Expect(c.EffectiveQuotaEntries()).To(HaveLen(1))
		}
	})
})
