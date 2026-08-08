package scalefromzero

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

const selNS = "chat"

// pools builds a single cluster-scoped provider with the given free capacity.
func pools(free map[string]int) []*pipeline.ResourceConstraints {
	p := make(map[string]pipeline.ResourcePool, len(free))
	for accType, avail := range free {
		p[accType] = pipeline.ResourcePool{Limit: avail, Used: 0}
	}
	return []*pipeline.ResourceConstraints{{ProviderName: "gpu-limiter", Pools: p}}
}

func cand(name, role, accel string, gpus int, cost float64) Candidate {
	return Candidate{
		VariantName:     name,
		ScaleTargetName: name + "-deploy",
		Role:            role,
		Accelerator:     accel,
		GPUsPerReplica:  gpus,
		Cost:            cost,
	}
}

// names extracts the selected variant names, order-independent comparison being
// the caller's job.
func names(set []Candidate) []string {
	out := make([]string, 0, len(set))
	for _, c := range set {
		out = append(out, c.VariantName)
	}
	return out
}

func hasAll(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// TestSelectExhaustivePairBeatsGreedy is the reason the search is exhaustive.
// The cheapest decode (d-cheap, 4xH100) leaves only 0 H100 free, so no prefill
// fits beside it; the feasible pair needs the COSTLIER decode (d-l4, 2xL4). A
// greedy "cheapest decode, then find a prefill" would report no pair and drop to
// decode-only, giving up prefill capacity that exists.
func TestSelectExhaustivePairBeatsGreedy(t *testing.T) {
	in := SelectionInput{
		Namespace: selNS,
		Candidates: []Candidate{
			cand("d-cheap", domain.RoleDecode, "H100", 4, 1),
			cand("d-l4", domain.RoleDecode, "L4", 2, 3),
			cand("p-h100", domain.RolePrefill, "H100", 2, 1),
		},
		Constraints: pools(map[string]int{"H100": 4, "L4": 2}),
	}

	set, outcome := selectServingSet(in)
	if outcome != OutcomePair {
		t.Fatalf("outcome = %q, want %q; a feasible pair exists and must be found", outcome, OutcomePair)
	}
	if got := names(set); !hasAll(got, "d-l4", "p-h100") {
		t.Fatalf("selected %v, want the only feasible pair [d-l4 p-h100]", got)
	}
}

func TestSelectServingSet(t *testing.T) {
	tests := []struct {
		name        string
		in          SelectionInput
		wantOutcome SelectionOutcome
		wantSet     []string
	}{
		{
			name: "non-disaggregated model wakes its cheapest variant",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("v-expensive", domain.RoleBoth, "H100", 1, 9),
					cand("v-cheap", domain.RoleBoth, "L4", 1, 2),
				},
				Constraints: pools(map[string]int{"H100": 8, "L4": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"v-cheap"},
		},
		{
			name: "P/D model wakes decode and prefill together when both fit",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomePair,
			wantSet:     []string{"d", "p"},
		},
		{
			name: "prefill dropped when the pair will not fit but decode alone will",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				// Room for one of them, not both: the decode wins because the
				// router runs prefill on the decode worker anyway.
				Constraints: pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "requirePrefill refuses a decode-only wake",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints:    pools(map[string]int{"H100": 2}),
				RequirePrefill: true,
			},
			wantOutcome: OutcomePrefillRequired,
			wantSet:     nil,
		},
		{
			name: "requirePrefill still wakes the pair when it fits",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				Constraints:    pools(map[string]int{"H100": 4}),
				RequirePrefill: true,
			},
			wantOutcome: OutcomePair,
			wantSet:     []string{"d", "p"},
		},
		{
			name: "nothing is woken when no decode fits",
			in: SelectionInput{
				Namespace:   selNS,
				Candidates:  []Candidate{cand("d", domain.RoleDecode, "H100", 4, 1)},
				Constraints: pools(map[string]int{"H100": 2}),
			},
			wantOutcome: OutcomeNoCapacity,
			wantSet:     nil,
		},
		{
			name: "a prefill-only population has nothing that can serve",
			in: SelectionInput{
				Namespace:   selNS,
				Candidates:  []Candidate{cand("p", domain.RolePrefill, "H100", 2, 1)},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeNoDecodeCandidate,
			wantSet:     nil,
		},
		{
			name: "a model already serving is left to the saturation engine",
			in: SelectionInput{
				Namespace:     selNS,
				Candidates:    []Candidate{cand("p", domain.RolePrefill, "H100", 2, 1)},
				DecodeCovered: true,
				Constraints:   pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     nil,
		},
		{
			name: "an already-running prefill is not duplicated",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d", domain.RoleDecode, "H100", 2, 3),
					cand("p", domain.RolePrefill, "H100", 2, 1),
				},
				PrefillCovered: true,
				Constraints:    pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "unknown constraints do not block the wake",
			in: SelectionInput{
				Namespace:  selNS,
				Candidates: []Candidate{cand("d", domain.RoleDecode, "H100", 128, 1)},
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d"},
		},
		{
			name: "equal-cost candidates resolve deterministically by name",
			in: SelectionInput{
				Namespace: selNS,
				Candidates: []Candidate{
					cand("d-b", domain.RoleBoth, "H100", 1, 5),
					cand("d-a", domain.RoleBoth, "H100", 1, 5),
				},
				Constraints: pools(map[string]int{"H100": 8}),
			},
			wantOutcome: OutcomeDecodeOnly,
			wantSet:     []string{"d-a"},
		},
		{
			name: "a namespace quota that excludes the accelerator blocks the wake",
			in: SelectionInput{
				Namespace:  selNS,
				Candidates: []Candidate{cand("d", domain.RoleDecode, "H100", 1, 1)},
				Constraints: []*pipeline.ResourceConstraints{{
					ProviderName: "quota-limiter",
					Pools:        map[string]pipeline.ResourcePool{"H100": {Limit: 64}},
					NamespacePools: map[string]map[string]pipeline.ResourcePool{
						selNS: {"L4": {Limit: 8}},
					},
				}},
			},
			wantOutcome: OutcomeNoCapacity,
			wantSet:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, outcome := selectServingSet(tt.in)
			if outcome != tt.wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if got := names(set); !hasAll(got, tt.wantSet...) {
				t.Fatalf("selected %v, want %v", got, tt.wantSet)
			}
		})
	}
}

// TestDemandOfSumsSharedAccelerator pins the reason demand is a map summed over
// the set: two variants on the same accelerator contend for one pool, which a
// per-variant feasibility check would miss entirely.
func TestDemandOfSumsSharedAccelerator(t *testing.T) {
	set := []Candidate{
		cand("d", domain.RoleDecode, "H100", 2, 1),
		cand("p", domain.RolePrefill, "H100", 3, 1),
	}
	demand := demandOf(set)
	if demand["H100"] != 5 {
		t.Fatalf("demand[H100] = %d, want 5 (2+3 on the same pool)", demand["H100"])
	}
}

// TestDemandOfFloorsMissingGPURequest mirrors GetTotalGPUsPerReplica's floor: a
// workload with no explicit GPU request still occupies one.
func TestDemandOfFloorsMissingGPURequest(t *testing.T) {
	demand := demandOf([]Candidate{cand("d", domain.RoleDecode, "H100", 0, 1)})
	if demand["H100"] != 1 {
		t.Fatalf("demand[H100] = %d, want 1", demand["H100"])
	}
}
