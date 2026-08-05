package domain

// VariantMetadata is the authoritative per-variant identity and replica/GPU
// state for one optimization cycle. It carries everything the optimizer and the
// analyzers need to know about a variant that is *not* a measured signal:
// identity (VariantName, ModelID, Namespace, Role, AcceleratorName), economics
// (Cost), and current fleet state (replica counts, GPUsPerReplica, bounds).
//
// It is produced by the discovery step (internal/engines/discovery), the single
// source of this metadata — replacing the previous scheme in which the
// saturation analyzer laundered identity onto its per-variant capacity output
// and cost/accelerator were stamped onto every pod's ReplicaMetrics.
type VariantMetadata struct {
	VariantName string
	ModelID     string
	Namespace   string
	// Role is the P/D disaggregation role: "prefill", "decode", or "both".
	Role string
	// Cost is the per-replica cost used for cost-aware variant selection.
	Cost float64
	// AcceleratorName is the GPU product the variant runs on, resolved from the
	// scale target's pod-template nodeSelector/nodeAffinity (identical for every
	// pod of the variant — a variant-level fact, not a per-pod one).
	AcceleratorName string
	GPUsPerReplica  int
	CurrentReplicas int
	// DesiredReplicas is the last optimizer decision recorded on the VA status,
	// 0 if none yet.
	DesiredReplicas int
	ReadyReplicas   int
	PendingReplicas int
	// MinReplicas/MaxReplicas are the scaling bounds; nil means unset.
	MinReplicas *int
	MaxReplicas *int
}

// ToReplicaState projects the metadata onto the VariantReplicaState the analyzers
// consume today. Identity/economics fields that VariantReplicaState has no home
// for (ModelID, Namespace, Cost, AcceleratorName, ReadyReplicas) are dropped
// here; consumers that need them read the VariantMetadata directly.
//
// This projection keeps BuildVariantStates behavior-identical while discovery
// becomes the single producer — the transitional step before analyzers and the
// optimizer switch to reading VariantMetadata.
func (m VariantMetadata) ToReplicaState() VariantReplicaState {
	return VariantReplicaState{
		VariantName:     m.VariantName,
		CurrentReplicas: m.CurrentReplicas,
		DesiredReplicas: m.DesiredReplicas,
		PendingReplicas: m.PendingReplicas,
		GPUsPerReplica:  m.GPUsPerReplica,
		Role:            m.Role,
		MinReplicas:     m.MinReplicas,
		MaxReplicas:     m.MaxReplicas,
	}
}
