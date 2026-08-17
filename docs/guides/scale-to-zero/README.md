# Scale a model to zero, and get it back

## Overview

Lets an idle model release its accelerators entirely, and brings it back when a
request arrives. Two mechanisms, and they fail independently:

- **Parking** is WVA's. When a model serves nothing for `retentionPeriod`, it
  scales every variant to zero.
- **Waking** is EPP's queue plus KEDA. A request for a model with no endpoints is
  held in EPP's flow-control queue; WVA reads that queue, publishes an
  activation, and KEDA scales the workload off zero.

Parking is the easy half and the dangerous one. A cluster can park a model
perfectly and be unable to wake it, and nothing about the park looks wrong — so
**check the wake path first**, before you let anything park.

Turning this on is two settings that must agree. Scale-to-zero enabled on the
model, and `minReplicaCount: 0` on **every** variant. Set one without the other
and you get a valid configuration that quietly does not do what it looks like it
does: WVA reports which half is missing rather than leaving you to find out from
the bill.

## Prerequisites

A model already serving under WVA — follow
[Install WVA in a namespace](../install-in-namespace/) first.

Then the wake signal, which is the precondition worth checking before any other:

<!-- guide:prerequisites.flowcontrol start -->
```bash
# Check the WAKE signal first. Parking a model is easy and waking it needs a
# queue that only exists when EPP has flow control enabled — so a cluster
# that fails this check will park a model and never get it back.
# Ask the EPP process what it PARSED, not what the ConfigMap says: EPP reads
# --config-file once at startup, so enabling the gate does not reach a pod
# that is already running.
kubectl logs -n <llmd-namespace> deploy/<epp-deployment> | grep -m1 -i featuregates
# want: featureGates:["flowControl"]   — if absent, or the pod predates the
# ConfigMap edit, restart it:
#   kubectl rollout restart -n <llmd-namespace> deploy/<epp-deployment>
```
<!-- guide:prerequisites.flowcontrol end -->

Ask the process, not the ConfigMap. EPP reads `--config-file` once at startup, so
a ConfigMap that gains `featureGates: [flowControl]` never reaches a pod already
running — and every artifact you would compare (image digest, ConfigMap, feature
gate, auth) still matches. That difference cost a week of failures blamed on the
autoscaler. It recurs on **every** ConfigMap edit, because the EPP Deployment
carries no `checksum/config` annotation to restart it.

<!-- guide:prerequisites.engine start -->
```bash
# One engine per model. Idleness is read from that engine's request counter —
# vllm:request_success_total or sglang:num_requests_total — and WVA asks for
# the one matching the engine it detects. A model running BOTH would need both
# counters summed, so it is refused rather than measured with half its traffic.
kubectl get deploy -n <llmd-namespace> -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].image}{"\n"}{end}'
```
<!-- guide:prerequisites.engine end -->

## Installation Instructions

**Half one — allow the model to park.**

<!-- guide:deploy.policy start -->
```bash
# Half one: allow the model to park. Absent, this inherits from the
# WVA_SCALE_TO_ZERO deployment flag.
kubectl edit configmap wva-scaling-policy-config -n <wva-namespace>
# under the entry this model resolves to (its scalingPolicy tier, or default):
#   scaleToZero:
#     enabled: true
#     retentionPeriod: 10m          # idle time before parking
#     initialCooldownPeriod: 300s   # how long WVA watches before it may park
#
# Time to zero is retentionPeriod + KEDA's cooldownPeriod, in sequence.
```
<!-- guide:deploy.policy end -->

The entry is the one this model resolves to: its `scalingPolicy` tier from the
ScaledObject's trigger metadata, or `default`. Policy is a **model** property, not
a variant one — a model is at zero only when nothing serves it, so there is
nothing meaningful a single variant could decide here.

**Half two — let every variant reach zero.**

<!-- guide:deploy.bounds start -->
```bash
# Half two: let every variant reach zero. A model is at zero only when
# NOTHING serves it, so one variant left at 1 keeps the whole model up and
# the policy above does nothing.
kubectl get scaledobject -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicaCount,MAX:.spec.maxReplicaCount
```
<!-- guide:deploy.bounds end -->

Every `MIN` must be `0`. One variant left at `1` keeps the whole model up, and the
policy above then does nothing at all.

**Then the trap that costs the most time.**

<!-- guide:deploy.hpa start -->
```bash
# THE TRAP. KEDA derives the HPA from the ScaledObject ONCE. An HPA created
# while minReplicaCount was 1 keeps minReplicas: 1 forever, so the workload
# cannot reach zero no matter what the ScaledObject says afterwards. Editing
# the ScaledObject in place is not enough — delete it and let KEDA rebuild
# the HPA.
kubectl get hpa -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicas
# any MIN of 1 for a variant you set to 0:
#   kubectl delete scaledobject -n <llmd-namespace> <name> && kubectl apply -f <its manifest>
```
<!-- guide:deploy.hpa end -->

KEDA builds the HPA from the ScaledObject once. An HPA created while
`minReplicaCount` was `1` keeps `minReplicas: 1` for the rest of its life, so the
workload cannot reach zero however the ScaledObject reads afterwards. Editing the
ScaledObject in place does not fix it; delete and re-apply so KEDA rebuilds the
HPA. Four separate attempts at this failed here and were misread as "WVA is
holding the model active".

## Verification

Start by asking WVA whether it thinks anything is in the way. Silence is the
healthy answer:

<!-- guide:verify.blocked start -->
```bash
# Ask WVA whether anything stops this model parking. No output is the healthy
# answer. Each reason names a CONTRADICTION between the two halves above.
kubectl port-forward -n <wva-namespace> svc/wva-controller-manager-metrics-service 8443:8443 &
curl -sk https://localhost:8443/metrics | grep wva_model_scaling_blocked
# variant-floor        a variant still has minReplicas > 0
# policy-forbids-zero  every variant permits zero, the policy does not
# engine-unsupported   the model runs BOTH vLLM and SGLang
# no-wake-signal       EPP exports no flow-control queue: it would not wake
```
<!-- guide:verify.blocked end -->

| reason | what to change |
| --- | --- |
| `variant-floor` | a variant still has `minReplicaCount > 0` — half two is incomplete |
| `policy-forbids-zero` | every variant permits zero but the policy does not — half one is incomplete |
| `engine-unsupported` | the model runs **both** vLLM and SGLang, so no single request counter measures it; either alone is fine |
| `no-wake-signal` | EPP exports no flow-control queue — **fix before letting it park** |

Then watch it park:

<!-- guide:verify.park start -->
```bash
# Stop sending traffic and wait out retentionPeriod. wva_model_replicas is
# the model's total across variants, so 0 means nothing is serving it.
kubectl get deploy -n <llmd-namespace> -w
curl -sk https://localhost:8443/metrics | grep wva_model_replicas
```
<!-- guide:verify.park end -->

Then prove it comes back. This drives the whole chain on a real cluster and says
which link broke, rather than reporting a wake that some other cause produced:

<!-- guide:verify.wake start -->
```bash
# The whole chain, end to end, on a real cluster: requests queue at zero
# endpoints -> WVA publishes an activation -> the workload leaves zero. A
# wake for some other reason is not a pass, so it checks the queue and the
# activation too, and names WHICH link broke. It parks the model itself
# first, and restores what it changed.
./hack/verify_scale_from_zero.sh <llmd-namespace> <deployment> <modelID>
```
<!-- guide:verify.wake end -->

It parks the model itself first, asserts the HPA precondition before anything
else runs, and restores what it changed. A wake caused by a floor, a manual
scale, or another controller is **not** counted as a pass — the queue depth and
WVA's own activation are checked alongside the replica count.

## Cleanup

<!-- guide:cleanup.disable start -->
```bash
# Set enabled: false and the model is held at one replica again. Leaving the
# variants at minReplicaCount: 0 is then a contradiction WVA will report as
# policy-forbids-zero — raise them back if you want the metric quiet.
kubectl edit configmap wva-scaling-policy-config -n <wva-namespace>
```
<!-- guide:cleanup.disable end -->

## When a model will not park

Two configurations are valid, cost money, and produce no symptom of their own —
the model is up and serving, exactly as every other metric says it should be:

| what you set | what actually happens |
| --- | --- |
| scale-to-zero enabled, one variant floored | that variant keeps serving, the model never reaches zero, the setting is inert — idle accelerators, billed indefinitely |
| every variant at `minReplicaCount: 0`, policy disabled | WVA holds the model up, and the bounds are inert |

The second is most misleading with a **single variant**, where `minReplicaCount: 0`
reads exactly like a deliberate request to park.

Neither is an error, and WVA does not reject either. It reports them on
`wva_model_scaling_blocked` and logs once when the answer changes, because the
thing that is wrong is your expectation, and nothing else will tell you.

## When a model will not wake

`no-wake-signal` means EPP is exporting no flow-control queue **at all** — not
that the queue is empty. An idle queue reports `0` and is healthy; a queue that
does not exist reports nothing, and a model parked behind it is stranded.

WVA reports this for **serving** models too, not just parked ones, and that is
the point. The idle check that parks a model is vLLM's request counter, which has
nothing to do with flow control — so WVA will park a model behind a
flow-control-less EPP and only then discover it cannot get it back. The warning
arrives while the model is still up and the fix is still cheap.

The usual cause is the stale pod described under Prerequisites. Restart EPP.

## Monitoring

| signal | means |
| --- | --- |
| `wva_model_scaling_blocked{reason}` | present only while that reason holds; absence is healthy |
| `wva_model_replicas` | replicas serving a model, across variants. `0` is normal for an idle parked model |
| `WVAModelWillNotScaleToZero` | info, 15m — a configuration contradiction, costing accelerators |
| `WVAModelHasNoWakeSignal` | warning, 10m — parked models here will not come back |
| `WVAModelParkedWhileRequestsRefused` | critical, 5m — at zero while requests are being refused, whatever the cause |

The last one is the one to page on: it alerts on the **symptom**, so it catches
causes nobody has enumerated, including ones not in the table above. Start from
`wva_model_scaling_blocked` for that model — if a reason is present, it is
probably the cause.

Two panels on the [operational dashboard](../../user-guide/monitoring.md) carry
the same data: *Models that will not scale, by reason* and *Models at zero
replicas*.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `scaleToZero.enabled` | inherits `WVA_SCALE_TO_ZERO` | `true` |
| `scaleToZero.retentionPeriod` | `10m` | `5m` |
| `scaleToZero.initialCooldownPeriod` | `300s` | `0` to disable the hold |
| `scaleFromZero.requirePrefill` | `false` | `true` |
| ScaledObject `minReplicaCount` | `1` | `0` — required on **every** variant |
| ScaledObject `cooldownPeriod` (KEDA) | `300s` | `30s` |

## How long parking actually takes

**Two timers run in sequence, and they add up.** This is the single most common
reason a fleet looks like it "will not park":

```
last request ──┬─ retentionPeriod ─┬─ ≤1 optimize interval ─┬─ cooldownPeriod ─┬─ 0 replicas
               │ (WVA decides)     │ (trigger goes inactive)│ (KEDA acts)      │
```

WVA reports the KEDA trigger active *until* it decides the model needs zero, and
it only decides that once the idle query over `retentionPeriod` reads zero. So
KEDA's cooldown cannot even begin until WVA is already done waiting. With both
defaults that is **10m + 300s ≈ 15 minutes** from the last request. Halving one
timer halves only its own share.

**And WVA will not park a model it has only just met.** The idle check reads
Prometheus *history*, not WVA's own observations — so a model that was already
quiet is parkable the instant WVA starts, on the strength of a window WVA was not
running for. Installing or restarting the controller on an idle fleet would
otherwise park it within one optimize interval.

`initialCooldownPeriod` (default `300s`) is the hold that prevents that: WVA must
have watched a model for that long before parking it. The clock is per model and
per process, so a restart restarts it. It is a **floor, not an addend** — the
first park lands at about `max(initialCooldownPeriod, retentionPeriod)`, and
steady-state timing is unchanged.

Every other autoscaler already behaves this way, measuring idleness from when it
started observing: KEDA from the trigger going inactive, Knative from its own
metric window, HPA from its stabilization window. Note the default is *not* KEDA's
`initialCooldownPeriod`, which is `0` — that would reproduce the very surprise
this prevents. Set `"0"` if you want the old behaviour.

`retentionPeriod` does double duty: it is how long a model must be idle before it
parks, and how long a just-woken model is held before the idle check may park it
again. Without that hold, a wake is undone before it can serve the request that
asked for it — the request is still queued in EPP while the pod pulls and loads,
so the counter reads idle for precisely the model that has demand waiting.

`scaleFromZero.requirePrefill` applies to P/D models: by default a decode variant
may be woken alone.

## Next

- [After the install](../../deployment/operations.md) — what to watch, first-line
  troubleshooting
- [Scaling policy configuration](../../developer-guide/scaling-policy-config.md) —
  every field of an entry, and how tiers resolve
- [Monitoring](../../user-guide/monitoring.md) — the dashboard and the full metric
  surface
