# Guide review: installing WVA into a namespace running llm-d

A step-by-step walkthrough of
[Install WVA in a namespace](../guides/install-in-namespace/README.md), run against a
real shared cluster, recording what a reader hits and fixing what could be fixed.

The **PR description** below covers everything fixed. **Still open** lists what was
found and deliberately not changed, with the reasoning.

| | |
| --- | --- |
| Cluster | `api-pokprod001-ete14-res-ibm-com:6443` — OpenShift 4.19.19, **shared**, ~100 tenant namespaces |
| Namespace | `dhl-la-1708`, llm-d from [`optimized-baseline`](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) |
| Controller image | `ghcr.io/ev-shindin/llm-scaler:main` @ `sha256:8793575b…f87a05e4` |
| Codebase | new fork, full refactor. Only `biran` and `evgensh-wva-test` run it, so older installs are not evidence about these scripts |
| Reference for comparison | `evgensh-wva-test` — a working install of this code, used as the positive control throughout |
| State as left | controller running; ScaledObject registered and **parked at 0** (`autoscaling.keda.sh/paused-replicas`), so no GPUs held; model-server PodMonitor in place; a private Grafana running in the namespace. `make so-resume` brings the workload back |

**The scenario, which governs every decision below:** llm-d already works in a single
namespace, and WVA is added to it. That is the default and must work out of the box.
Cluster-wide WVA is a different shape with different preconditions, selected by
`WVA_SCOPE`, and is out of scope here.

**The unifying defect.** Every gap found produced the *same* visible symptom:
controller `Running`, ScaledObject `READY True`, HPA at a healthy `1/1 (avg)`, and
nothing autoscaled. WVA's characteristic failure is silence, so the checks exist to
**enable** a working install — name what is incomplete, as early as the reader can act
on it, and point at llm-d when llm-d is what needs finishing.

---

# PR description

## Make a WVA install fail loudly instead of silently doing nothing

Walking the install guide end to end on a shared OpenShift cluster surfaced a family
of defects with one shape: the install succeeds, every status reads healthy, and
nothing is ever autoscaled. Each fix below closes one of those, and the preflight
now refuses rather than installing something that cannot work.

Nothing here changes what WVA decides. It changes what it can *see*, and what it says
when it cannot see.

### Discovery: WVA could not find llm-d's own model servers

- **Serving marker.** Discovery matched only `llm-d.ai/inferenceServing=true`, which
  llm-d's guides never set — `guides/recipes/modelserver/base/single-host/default`
  sets `llm-d.ai/role: decode`, and `optimized-baseline` adds model/guide/accelerator
  labels. On a namespace built from the guide the plan came out empty while every step
  reported success. Now accepts `llm-d.ai/role in (decode, prefill)`; `requester` stays
  excluded because an FMA requester holds no engine.
- **`modelID`.** `so_model_id` read `--served-model-name` then `--model`.
  `optimized-baseline` passes the model **positionally** (`vllm serve Qwen/Qwen3-32B`),
  so every entry was written `apply: no`, "the model could not be read". Adds the
  positional forms, including `sh -c "vllm serve <model> …"`.
- The marker was hardcoded at **five** sites across two files; fixing four left the
  preflight contradicting itself, listing a namespace in its own "look elsewhere" list.
  All five now share one definition.

### Metrics: the install could not see what it sizes from

- **Model-server scrape.** WVA's capacity model is built entirely on `vllm:*` series.
  llm-d ships the PodMonitors, but as **"### 3. (Optional) Enable monitoring"** —
  optional for llm-d, required for WVA. A correctly-followed llm-d install can leave a
  namespace with zero `vllm` series; this one ran four replicas for hours and produced
  none. Now verified, reported with llm-d's own command, and a **stop**.
- **EPP signals**, which are two separate requirements with different fixes:
  `inference_extension_scheduler_attempts_total` needs the EPP **scraped** and feeds the
  arrival rate and so the throughput analyzer; `…_flow_control_queue_size` needs the
  **`flowControl` gate** and feeds scale-from-zero and `wva_unmeasured_queue`. Neither
  implies the other and this namespace had neither. Both are now checked and are a
  **stop**, citing `guides/flow-control/tuning.md`.
- **`wva_unmeasured_queue` is sourced from the EPP queue**, so with the gate off the
  detector for "serving through pods WVA cannot attribute" reads 0 forever — the safety
  net is disabled by the absence it exists to catch.

### The two-phase install broke its own observability

`ServiceMonitor` is an admin-phase object (a namespace admin cannot create
`monitoring.coreos.com` resources — the reason the admin phase exists), while its
`bearerTokenSecret` was a controller-phase object. So the ServiceMonitor was created
33 minutes before the Secret it references, the prometheus-operator **rejected** it as
`InvalidConfiguration`, and never re-evaluated: a metadata write does not re-trigger
it and re-applying identical content is a no-op. WVA's own metrics were permanently
uncollected while the install printed "All components verified successfully!".

The single-command install was unaffected, which is why it survived — it is the
two-person split, the shape the guides recommend, that broke. Both namespaces running
this code with working metrics had their Secret created **3 seconds before** their
ServiceMonitor; this one had it 33 minutes after.

ServiceAccount and Secret move into the prereqs phase so the three interdependent
objects land together, and `verify_deployment` now reports a rejected ServiceMonitor —
checking both the missing Secret and a standing rejection, because an install that
*creates* the missing Secret is still not scraped.

### Preconditions, driven by scope

`WVA_SCOPE` already selected the scenario, so it is the only control; the per-check
`WVA_ALLOW_*` flags that were briefly added are gone and `SKIP_CHECKS=true` is the
single bypass, which still prints the findings.

- **`namespace` is now the default on every platform.** It was inferred from the
  platform — the code called it "the historical inference" — so `deploy/install.sh` on
  Kubernetes or kind silently selected the cluster scenario. The old inference is left
  in place, commented and unreached.
- **`NAMESPACE` is mandatory in namespace scope.** It names what WVA *manages*;
  `WVA_NS` names where the *controller installs*, and they differ only in the
  cluster-wide shape. Previously `WVA_NS` fell back to
  `workload-variant-autoscaler-system` and discovery then silently adopted whichever
  namespace ran model servers — both could install a healthy controller into a
  namespace with nothing to scale.
- **Stops:** namespace absent, no llm-d model servers, model servers unscraped, EPP
  signals missing. A workload parked at 0 replicas still counts — the count reads the
  pod template, so `so-park`, scale-to-zero and mid-rollout are invisible to it.
- **Informational:** KEDA ScaledObject CRs. Objects to scale are WVA's *premise*;
  ScaledObjects are its own *output*, so none yet is the best possible state — exactly
  what a namespace looks like before registration — and some already present is fine,
  since the plan offers `adopt` per entry.
- **Cluster scope is not disabled**: it warns that it is WIP and points at
  `SKIP_CHECKS=true`. Its discovery code stays, because finding the namespaces to
  manage is its actual purpose.

### Operating a WVA-managed workload

`so-park`, `so-freeze`, `so-resume`, `so-list`. Idling one is not obvious and every
obvious move fails — `kubectl scale --replicas=0` is restored by the HPA within
seconds, `maxReplicaCount: 0` is not a valid state, and scaling the **controller**
down is worse than nothing because KEDA then falls back to a CPU metric and keeps
sizing the workload by the wrong signal while everything reads healthy. The supported
lever is `autoscaling.keda.sh/paused-replicas` on the ScaledObject; the controller
stays up, holding no GPU.

All three ask which ScaledObject (`SO=<name>|all` skips the prompt and is required
without a terminal), select by trigger address so an `apply: adopt` object is included,
and report the GPUs released. They deliberately do **not** replay the pre-park replica
count on resume: `minReplicaCount` plus WVA's decision from live metrics should govern.

### Smaller fixes

- The `cluster-monitoring-view` binding for the user-workload Prometheus SA is removed.
  It was introduced to let UWM scrape the controller's `/metrics`, and cannot: that
  role carries no `nonResourceURLs` rule. The scrape is authenticated with WVA's own SA
  token plus `metrics-reader`, and discovery is UWM's own. It granted the UWM
  Prometheus SA unrestricted platform-API read, from a WVA install, for no effect.
- `wva_installations` made one extra API call per install to read args the list
  response already contained — 12 installs, ~13s of preflight, for nothing.
- The candidate-namespace search is offered rather than performed: it is what a
  namespace admin may not do, and pure cost for someone who simply forgot the variable.

## Testing

Verified on the live cluster, using `evgensh-wva-test` as a positive control
throughout — every check was confirmed to both fire and *not* fire.

- Discovery and `modelID`: the plan now finds
  `optimized-baseline-nvidia-gpu-vllm-decode` and reads `Qwen/Qwen3-32B`, **at
  `replicas=0`**, confirming discovery reads the pod template and not live pods.
- `modelID` parsing: 9 argument shapes, including both flag spellings, the positional
  form, `sh -c` wrapping, and the cases that must return empty rather than guess.
- ServiceMonitor ordering: reproduced the fault by deleting the ServiceMonitor and
  Secret, then ran the prereqs phase alone — Secret then ServiceMonitor, one apply, no
  rejection, `up=1` and 17 `wva_*` series where there had been none.
- EPP checks: `dhl-la-1708` reads not-scraped/gate-off and the preflight exits 2;
  `evgensh-wva-test` reads scraped/gate-on and passes. Two bugs were caught this way —
  ServiceMonitors select **Services**, not pods, and each object was being fetched
  several times.
- Scope and namespace: resolves to `namespace` for `kubernetes`, `openshift`,
  `kind-emulator` and unset; no `NAMESPACE` stops with a capped candidate list;
  `SKIP_CHECKS=true` reports the same findings and passes.
- `so-*`: `so-list` renders state, re-parking is a no-op, an unknown `SO=` fails naming
  it, no-terminal-without-`SO=` refuses instead of hanging, and freeze-on-parked
  refuses and changes nothing. **Also exercised against a live workload**: `so-resume`
  removed the pause and KEDA brought the deployment back to `minReplicas: 1`, and
  `so-park` reported "releasing 2 GPU(s)" and did.
- **The whole loop, end to end.** With one replica serving, 145 completions were driven
  at the pod (`/v1/models` then `/v1/completions`), and every stage was confirmed in
  turn: vLLM counted them (2315 prompt / 17305 generation tokens), the PodMonitor
  delivered them to Thanos (`rate(vllm:generation_tokens_total[5m])` = 60.4 tok/s), WVA
  read them (`wva_saturation_metrics_up` = 1, `wva_metrics_pods_discovered` = 1), and
  emitted `scaling-decision … "action":"no-change"` — correct for one replica at that
  load. This is the first time the full chain worked in this namespace, and it is what
  the discovery, `modelID` and model-server-scrape fixes together buy.
- Automated tests were analysed rather than run, and three callers would have broken:
  `deploy-e2e-infra` (calls `install.sh` with no `NAMESPACE` — `kind-emulator` is
  exempt), `undeploy-wva` (passes only `WVA_NS`, correctly — so the requirement runs on
  the check and install paths only), and `nightly-deploy-wva-guide` (set only `WVA_NS`;
  now passes `NAMESPACE` explicitly, preserving its behaviour).

`make check-prereqs` on `dhl-la-1708`: ~2min → 50s, with identical findings.

## Commits

| | |
| --- | --- |
| `ec14306c` | read the positional model of `vllm serve <model>` |
| `d1b16ab8` | discover model servers by `llm-d.ai/role` |
| `8b28ec4d` | count model servers by the shared marker list |
| `1c83c42a` | apply the ServiceAccount and token Secret in the prereqs phase |
| `71f3d2cb` | report a ServiceMonitor the prometheus-operator has rejected |
| `0939e549` | drop the `cluster-monitoring-view` binding for the Prometheus SA |
| `903d83fe`, `59900a28`, `02612181` | verify the model servers are scraped; report, do not create |
| `56e0eb7a`, `e97c1f67` | check the EPP's `flowControl` gate and scrape, and refuse without them |
| `600c284e` | `so-park`, `so-freeze`, `so-resume`, `so-list` |
| `1fb4f8db` | default to namespace everywhere, require `NAMESPACE` in it |
| `cce5cb38` | stop when llm-d is incomplete; ScaledObjects are not a precondition |
| `6eab56c0` | fold the per-install args read; scan for namespaces on demand |
| `17b4800a` | revert of a first attempt that inverted the policy |

## Gap IDs

The walkthrough numbered findings as it went, and the commit messages use those
numbers. Fixed ones are described above by what they are; this is the crosswalk.

| | |
| --- | --- |
| G-08 | serving marker — discovery found no model servers |
| G-09 | `modelID` — positional `vllm serve <model>` |
| G-10 | preflight exited 0 on a namespace that could not work (reworked: see *Preconditions*) |
| G-11 | the marker hardcoded at five sites |
| G-14 | `cluster-monitoring-view` bindings — Prometheus SA one removed, controller's kept for port 9091 |
| G-16 | ServiceMonitor created before its Secret |
| G-18 | model-server scrape never verified |
| G-19, G-20 | accelerator warning — see *Still open* |
| G-21 | PodMonitors left by undeploy — see *Still open* |
| G-01–G-07, G-12, G-13, G-15, G-17, G-22 | see *Still open* |

---

# Still open

Recorded, deliberately not changed. Documentation-only unless noted.

## Decided: leave as is

**PodMonitors are not WVA's to remove.** `undeploy-wva` leaves behind PodMonitors
applied into the workload namespace, including the FMA one the installer creates when
`WVA_FMA_LAUNCHER_METRICS=true`. Monitors are the user's to create as part of llm-d
setup and WVA should not delete them; a leftover monitor is harmless. *(was G-21)*

**A missing EPP queue refuses rather than degrades.** Refusing is right for now — the
gate is needed for scale-from-zero anyway. Degrading knowingly (run on engine metrics
alone, disable scale-from-zero, say which capabilities are off) is the better long-term
answer and is noted in the code.

**Preflight duration.** 51 sequential API calls, 47s of the 50s total; **42 are
namespaced gets against the single namespace and exactly one is cluster-wide**. The
floor is round-trip count × ~600ms latency to this cluster, not scope — on a local
cluster the same calls take seconds. Batching `wva_missing_prereqs` (18 per-object gets,
17s) would help but trades a readable existence check for name-set bookkeeping. Caching
was rejected as overkill. ~1 minute is acceptable.

## Decided: ignore the accelerator warning for now

*(was G-19, G-20)*

Resolution of a variant's accelerator only matters when a **per-accelerator limiter**
is declared, and the shipped policy declares none — its own ConfigMap says so: "the
list is the whole answer… declaring none — as shipped — means nothing is limited."
Without a limiter an unresolved accelerator is permissive (`FitsGPUBudget` asks whether
*any* pool has room); the cost is only that accelerator-keyed metrics are withheld.

And enabling the limiter is what fixes it: declaring `gpu-inventory` flips
`ObserveAccelerators` to `FromNodes`, and
[admin-gpu-bounding](../guides/admin-gpu-bounding/README.md) already requires granting
the per-namespace node access that makes node-based resolution work. So the path that
creates the requirement also supplies the mechanism.

Nothing to do. Two facts kept for whoever picks up the limiter work:

- **Node-based resolution needs running pods** ("the node its pods are running on"), so
  a workload parked at zero still will not resolve from nodes — a GPU product key in
  `nodeSelector` is the only source that works at zero replicas, which matters for
  scale-from-zero placement onto a heterogeneous cluster.
- **The event path is dead and its error line is load-bearing for one guide.**
  `emitAcceleratorNotResolvedEvent` records on a synthesized `VariantAutoscaling`, whose
  kind is not registered in the scheme since the CRD was removed, so the event never
  reaches the API server — `policy_report.go` records that an e2e run attempted 31 and
  none arrived, which is why it logs instead. The failed emission still retries every
  cycle, at `E` level. Measured here: `kubectl get events | grep -c
  AcceleratorNotResolved` = 0, with ~3 error lines a minute while the workload ran. The
  live diagnostic is `policy_report.go`'s prose line, deduped per change. See the
  documentation table for the guide that greps the wrong one of the two.

## Open work

**`make dashboard` — shipped.** `deploy/lib/dashboard.sh` + the `dashboard` Makefile
target productize what `config/grafana-private/` proved by hand: require the
grafana-operator CRDs and say so plainly if absent; apply a `Grafana` CR, a dedicated
ServiceAccount + token Secret, and the namespaced RBAC the Thanos tenancy port actually
checks (`create` on `pods.metrics.k8s.io`, plus `view` for ordinary reads); a
`GrafanaDatasource` on **port 9092** with `namespace=$NAMESPACE`, so no cluster-scoped
grant is needed; reuse `install_operational_dashboard` with `DASHBOARD_NS=$NAMESPACE`
(called directly now, so the `INSTALL_PHASE=prereqs` workaround the by-hand version
needed no longer applies); then a `GrafanaDashboard` CR importing that ConfigMap. Prints
the direct dashboard link and the password-retrieval command on **every** invocation,
including when everything already exists.

One real bug caught while building it, worth keeping as a note for the next person who
touches this: the datasource's auth ServiceAccount must **not** be named
`${grafana-cr-name}-sa` (i.e. `wva-grafana-sa`) — that is grafana-operator's own
auto-created ServiceAccount for the Grafana *pod itself*, owned by the Grafana CR.
`kubectl apply`'s 3-way merge tolerated the collision (labels and ownerReferences
survived), but it silently layered a second, unrelated purpose onto an object the
operator considers its own. `config/grafana-private/` had already avoided this by
naming it plain `grafana-sa`; the productized version uses an explicit
`${name}-datasource` suffix instead of relying on that being remembered.

Verified end to end on `dhl-la-1708`: `make dashboard` run twice reports everything
`unchanged` on the second run with identical URL/password output; the datasource health
check returns `Successfully queried the Prometheus API`; a real query against
`wva_config_info` returns data; asking for another namespace explicitly
(`evgensh-wva-test`) returns zero series, confirming tenancy-port isolation still holds
through the productized path.

**Tighten the datasource's TLS — attempted, reverted, root cause still open.** Tried
`jsonData.tlsAuthWithCACert: true` + `secureJsonData.tlsCACert` sourced from the
`openshift-service-ca.crt` ConfigMap every namespace gets auto-injected (the same CA
WVA's own ServiceMonitor mounts, for this exact certificate). It failed, and was
verified rather than assumed to have failed for a real reason:

- the CA content itself is right — `openssl verify -CAfile <that CA> <thanos-querier-tls's
  tls.crt>` returns `OK`;
- tried sourcing the same CA from a Secret instead of the ConfigMap, in case this
  operator version only resolves one `valuesFrom` source kind for `secureJsonData` —
  identical failure either way;
- restarted the Grafana pod to rule out a cached HTTP client from before the change —
  identical failure after a clean start.

Every attempt failed with the same error, `tls: failed to verify certificate: x509:
certificate signed by unknown authority`, while the request authenticated successfully
(rejected on TLS, not on auth) — so this is not a wrong CA, a bad token, or a caching
artifact; `tlsAuthWithCACert` does not take effect for this grafana-operator / Grafana
combination, for a reason not yet identified. Reverted to `tlsSkipVerify: true`, which
is proven working end to end and is what the reference configuration on this cluster
already does for the same certificate.

**Found as a side effect, worth its own line: this Grafana has no PVC.** A pod restart
wipes ALL datasources and dashboards — Grafana's storage is ephemeral — until
grafana-operator's next reconcile re-pushes them from the CRs, which happens
automatically but is not instant. `make dashboard` re-running is the reliable way to
confirm they came back; its apply is idempotent for exactly this reason. Worth deciding
later whether a small PVC belongs in the `Grafana` CR's spec, trading a bit of
persistence setup for not depending on the operator's reconcile loop after every pod
churn.

**Benchmark, as its own task.** `make benchmark-run BENCHMARK_WORKLOAD=decode_heavy`
does not work here as shipped, for three reasons found by reading it:

- the `llmdbenchmark` CLI is not installed, and `benchmark-install` is mostly llm-d
  standup automation — a `uv pip install` of the CLI is the wanted path instead;
- the scenario should be copied from `test/benchmark/scenarios/` when the target runs;
  today the copy lands in `$(BENCHMARK_REPO_DIR)/workload/profiles/<harness>/`, which
  assumes the clone;
- the endpoint is wrong for a real llm-d stack: the direct-KEDA branch hardcodes
  `infra-llmdbench-inference-gateway.<ns>.svc`, while an `optimized-baseline` namespace
  has `llm-d-inference-gateway-istio`.

Use an **inference-perf** decode-heavy scenario rather than this repo's
`prefill_heavy`. Related: this namespace's Gateway reads `PROGRAMMED False` with no
address, so anything dialling the gateway rather than the pod will fail until that is
sorted — the load above went straight to the pod IP.

**Untraced:** `wva_errors_total{error_type="Failed to scrape pod"}` reached 2930 while
the workload ran. Plausibly WVA's direct EPP scrape (it reads a token from
`/var/run/secrets/epp-metrics/token`) failing against an EPP that publishes nothing,
but that is a guess and has not been checked.

## What the missing EPP wiring looks like on the dashboard

Worth recording because it is the first *user-visible* consequence of the EPP gap, and
it presents as WVA malfunctioning when it is not:

```
wva_metrics_collection_errors_total{query_type=arrival_rate, reason=unknown} = 785
```

The arrival-rate query is PromQL over `inference_extension_scheduler_attempts_total`,
an EPP metric. With nothing scraping the EPP the series cannot exist, so every
collection cycle records an error, and the dashboard's error panel climbs steadily. It
is a faithful report of missing wiring rather than a fault in WVA.

Measured in this namespace: gate `off`, no scrapers, and `0 series` for both
`inference_extension_flow_control_queue_size` and
`inference_extension_scheduler_attempts_total`. So the panels fed by EPP data — arrival
rate, and anything derived from the scheduler queue — stay empty while the `vllm:*` and
`wva_*` panels populate normally.

## Documentation

| | |
| --- | --- |
| **G-01** | The README's five-command block reads as one linear script, hiding that `setup-prereqs` is a cluster-admin act with cross-tenant consequences. Mark the boundary inline, not in prose below. |
| **G-02** | An empty or wrong namespace used to install cleanly; now a stop. The remaining half is that the guide explains the failure *after* the command that would hit it. |
| **G-03** | `docs/guides/env.sh` defaults `IMG` to a floating `ghcr.io/ev-shindin/llm-scaler:main` — a pre-release build of an unmerged branch, and the file's own comment explains released images reject the flags these manifests pass. Pin to a digest while keeping a default that works. The digest used here is recorded above. |
| **G-04** | The admin step's cluster-scoped grants are not enumerated where an admin consents to them. Rendered, `namespace-scoped/openshift` produces 4 ClusterRoles and 5 ClusterRoleBindings; the only one exposing other tenants' data is `cluster-monitoring-view`. `manager-role` is a namespaced **Role** here — reading `config/base` suggests otherwise, and this review initially got that wrong. Wants an appendix generated from `kustomize build`. |
| **G-05** | On OpenShift, `SCOPE=namespace` still requires cluster-admin — stated only in a kustomization comment, not in the guide a reader follows. |
| **G-06** | `WVA_WATCH_NS` must be passed to two commands or the controller crash-loops; enforced only by prose. Multi-tenant, so not assessed here. |
| **G-07** | Nothing tells a reader how to confirm `modelID` before applying starts live scaling. `scaledobjects-plan` derives it correctly now, but the confirmation step is unstated. |
| **G-12** | The overlay's own comment claims it renders 3 ClusterRoles and 5 bindings; it renders 4 and 6. This review quoted the comment and inherited the error. Generate the numbers or check them in CI. |
| **G-13** | Three naming conventions for WVA cluster objects coexist on this cluster, including bare un-suffixed ones predating the per-namespace rename — one of which still grants another tenant's controller its permissions. An admin cannot tell a live object from an orphan, and `undeploy` only removes suffixed names. |
| **G-15** | In namespace scope the overlay renders no Namespace, so the `openshift.io/user-monitoring` patch is inert and never reaches the namespace; the install summary nevertheless lists `Namespace` as applied, because it prints `WVA_PREREQ_KINDS` rather than what was applied. Harmless here — UWM discovers ServiceMonitors without the label — but the comment claims otherwise. |
| **G-19/20** | `admin-gpu-bounding/README.md:23` checks accelerators resolve with `grep -i AcceleratorNotResolved` — the event *reason*, which appears in the log only inside the failed event emission. `gpu-limiter.md:197` greps `"Accelerator not resolved"`, the prose the working path actually logs. One of the two is wrong, and the wrong one is in the guide a reader follows before enabling a limiter. Point it at the prose form; after that the dead recorder call can be removed freely. |
| **G-17** | The install summary prints "Grafana: deployed in openshift-user-workload-monitoring" twelve lines after the check reported there is none, and "KEDA: installed or already present" after reporting no operator matched. Print what the check found. |
| **G-22** | The operational dashboard is published as a ConfigMap labelled `grafana_dashboard=1` — a **sidecar** convention — into `MONITORING_NAMESPACE`, where on OpenShift no Grafana ever runs. It sat unread for four days. grafana-operator ignores labelled ConfigMaps; it imports what a `GrafanaDashboard` CR points at. And the dashboard is only installable as a side effect of the prereqs phase, so the person who wants one cannot install it. **Fixed** — `make dashboard` (see *Open work* above) ships this; `config/grafana-private/` is retired in favour of it. |

## Deferred: registration at cluster scale

Registration is per-workload — `scaledobjects-plan` writes an entry per model server, a
human edits it, `scaledobjects-apply` creates one ScaledObject each. Tractable for one
namespace. On this cluster, corrected discovery finds model servers in ~100 namespaces,
and a hand-edited plan enumerating every workload, reapplied as workloads churn, is a
different problem. "Install WVA cluster-wide" cannot be answered by scaling the
namespace path unchanged.
