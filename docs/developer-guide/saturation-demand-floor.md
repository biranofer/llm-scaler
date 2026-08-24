# The arrival-rate demand floor

Why the saturation analyzer floors its demand at what the offered load requires,
what that floor is and is not invariant to, and which of its claims are measured
versus assumed.

Code: `internal/engines/analyzers/saturation_v2/arrival_demand.go`.

## The problem

Saturation's demand is **occupancy**: resident KV plus the waiting-queue
footprint. Both are states of the *fleet* rather than properties of the *load*,
and both shrink as capacity grows — resident KV because residence time falls, the
queue because it drains. The signal that sizes the fleet is therefore a function
of the fleet, and once the fleet is adequate the signal decays toward zero and
takes the replica count with it.

Measured on an H100 at 14 QPS (run `biran-20260822-021153-340`): demand fell from
7.5M tokens to 152k as ten replicas drained a 910-deep queue, and the target
followed from the ceiling down to one, mid-load. The arithmetic was right; the
input stopped describing the workload once the workload was being served.

## The floor

By Little's law:

```
L      = λ × W                requests concurrently in service
demand = L × (avgIn + avgOut) the KV they hold
demand = max(occupancy, that) never lowers, only raises
```

λ is the arrival rate the scheduler reports; the token counts are the request
shape. Neither moves when replicas are added, which is what makes them usable
where occupancy is not.

`W` is the per-request **service** time with the queue wait removed. End-to-end
latency will not do: it climbs when the fleet is behind and falls when it catches
up, which is the capacity-dependent term the floor exists to exclude.

## What it is not invariant to

`W` is not fully invariant. Inter-token latency rises with load, and service time
carries the same effect, so a starved fleet measures a longer `W` and asks for
more than it will need once it has it. The fleet arrives, `W` falls back, and the
floor relaxes.

That is a real overshoot on the **ramp**, in the same shape occupancy overshoots.
What it is not is the **collapse** the floor exists to prevent: `W` is bounded
below by the uncontended cost of the work, so the floor cannot decay toward zero
the way occupancy does. Damping the collapse without damping the ramp is the
trade, and it is deliberate.

Two further properties of `W` worth knowing:

- It is **completion-weighted and lags**. `avg_service_time` is
  `rate(sum)/rate(count)` — a mean over requests *completing* in the window. When
  the mix shifts toward longer requests, the completions in-window are the short
  ones that started recently, so `W` under-reads while the fleet fills with
  expensive work. "W tracks current load" is weaker than it sounds.
- A prefix-cache-heavy window does not collapse it: cache hits skip prefill, but
  decode dominates service time (0.055s against 24.5s measured), so `W` barely
  moves.

## Why it binds more often than "floor" suggests

`avgIn + avgOut` prices a request at its **peak** — a request holds its input for
its whole life while its output accumulates, so the KV it occupies averaged over
its lifetime is nearer `avgIn + avgOut/2`. The peak is this analyzer's existing
convention (see `waitingQueueDemand`, which calls I+O "a request's KV footprint at
its LAST decode step, not its mean"), so against a mean-measuring occupancy the
floor sits about 11% high and binds routinely rather than only during a collapse.

**The 11.3× gap observed on that run is not explained.** It is far larger than the
11% convention bias, and an earlier draft of this document proposed that occupancy
is a point-in-time gauge while the floor derives from windowed means. That is
wrong in both premise and direction: occupancy's KV usage is
`max_over_time(vllm:kv_cache_usage_perc[1m])`
(`internal/collector/registration/saturation.go`), windowed over the *same* minute,
and it takes the **peak** while the floor's inputs are means — which biases
occupancy *upward* and so predicts the opposite sign. What accounts for the
remaining gap is an open question, and until it is answered the floor is
correcting a discrepancy whose cause is unknown.

## Interaction with the conceded-replica clamp

A separate change caps supply at the replica count an in-flight scale-down has
already committed to (`steadystate.clampReplicaCountToScaleTarget`). Whether it is
present depends on the branch: it is NOT on `feat/arrival-rate-demand-floor`, and
it IS on `feat/clamp-plus-floor`, where the two run together for the first time.
Check before relying on anything below.

If both land they push the same way — the clamp lowers supply, the floor raises
demand, and `RequiredCapacity = demand/scaleUp − anticipatedSupply` responds to
both. Worked on the numbers from the run above (floor 1,722,000, `scaleUp` 0.85,
`prc` 550,758, an in-flight scale-down at `curr=3` against 5 still-reporting
replicas): each alone leaves RC at zero, while both together give RC ≈ 373,608,
or **about two-thirds of a replica** — enough to ask for one back and reverse the
scale-down in progress.

That is the intended damping rather than a defect, but it is **emergent**: neither
change produces it alone, so neither branch's tests can catch it. It needs an
integration test at whatever point both mechanisms are present.

## Claims in this document, and their status

| Claim | Status |
|---|---|
| Demand fell 7.5M → 152k as the queue drained | Measured, run `biran-20260822-021153-340` |
| `W ≈ avgOut × ITL` reproduces measured service time to 0.2% | Measured (24.60s vs 24.66s), decode-dominated shape only |
| Prefill was 0.055s of 24.6s | Measured, same run |
| Peak-vs-mean convention biases the floor ~11% high | Arithmetic from the request shape |
| Observed floor/occupancy gap of 11.3× | Measured; **cause unexplained** |
| `W` inflates under contention | Directionally established; the specific magnitude at one replica is **not** measured |
| Clamp + floor gives RC ≈ two-thirds of a replica | Arithmetic on logged values; **not** reproduced on a cluster |
