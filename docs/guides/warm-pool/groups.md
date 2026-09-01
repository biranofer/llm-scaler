# Pools of groups: engines that span machines

[← Warm pool guide](README.md)

## When the engine spans machines: pools of groups

A model too large for one machine runs as a **LeaderWorkerSet**: several Pods
holding one engine, with the API on the leader. A pool can hold those too, as
standing groups rather than standing Pods.

```bash
deploy/warmpool.sh create -n <namespace> --name h200-2pod   --group-size 2 --gpus 8   --accelerator NVIDIA-H200-141GB   --models 2 --model-size 70B   --wva-namespace <where WVA runs>   --launcher-image ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless
```

`--group-size` is what makes it a group pool. Everything else means what it did:
`--gpus` is devices **per Pod**, so the warm unit above holds 2 x 8 = 16.

### A group needs a patched supervisor image

`--launcher-image` is **required** here, and `create` refuses a group pool
without it.

The stock launcher runs every rank through vLLM's OpenAI API server, which knows
nothing about multi-node rank — grep it for `headless` or `node_rank_within_dp`
and you find neither. So the follower parses `--headless`, ignores it, builds a
full engine core, and dies:

```
AssertionError: collective_rpc should not be called on follower node
```

That call sits at the top of engine-core init and is not conditional, so no
combination of flags avoids it. vLLM's own CLI has always handled this
(`run_headless` sends `node_rank_within_dp > 0` to a bare `MultiprocExecutor`);
the launcher simply never reached that branch. The fix is one branch in
`launcher.py`, carried in the
[fork](https://github.com/ev-shindin/llm-d-fast-model-actuation) as *Route a
follower rank to the headless executor, not the API server* (`65ba31b`).

A build of it is published, so `--launcher-image` has a working value without
building anything:

```
ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless
sha256:e836bcf5bfa1268b5ec1c8027ba9732e2cafff11ab554dd8dbac67b110c65faf
```

It is `vllm/vllm-openai:v0.26.0` with the fork's `launcher.py` on top, and that
file is byte-identical to the readable copy in `warmpool/supervisor/` (checked by
extracting it from the published image). Pin the digest if you want the image to
stay put; build your own from the fork if you would rather not depend on it.

Refused rather than warned about, because the failure is silent in the worst
way: the group schedules, goes Ready, holds every one of its accelerators, and
every admission times out with the engine never answering.

Measured on two H100 nodes with vLLM 0.26.0 once the fix is in place — driven
through the launcher exactly as the pool drives it:

| | |
| --- | --- |
| leader | serves `/v1/completions`, `is_sleeping: false` |
| one `/sleep?level=1` to the **leader** | 78,015 MiB -> 2,745 MiB on **both** ranks |
| sleep | 0.71 s |
| wake | 0.36 s |

The sleep number is the one that makes a multi-node warm pool worth having: a
single call to rank 0 releases the GPU on every node, so a model that spans
machines is held warm for about 2.7 GiB per rank rather than a full set of
cards.

### A group serves exactly one shape

A group's `size` is fixed when the group is created -- it is the engine's shape,
not a scaling knob. So a group of 2 serves only models declaring `--nnodes 2`,
and WVA declines anything else **permanently**, saying which:

```
spans 4 Pod(s), this warm unit is 2
```

That holds even when the device totals agree. Sixteen GPUs across two Pods and
across four are the same count and a different engine. If you run both layouts,
you need a pool for each -- exactly as two accelerators need two pools.

### What is different once a pool holds groups

- **Only the leader is lent.** It runs the supervisor and serves the API; workers
  hold devices and join its process group. WVA never labels a worker into an
  InferencePool, because a labelled worker takes traffic nothing answers.
- **A group is all-or-nothing.** One Pod not Ready and the whole group drops out
  of the observation: ranks that cannot form are not a degraded engine, they are
  no engine. It reappears when the group is whole.
- **Memory is still per Pod.** `--models`/`--model-size` set each member's limit,
  because a level-1 sleeper's weights are charged to every member's own cgroup.
  The warm-set budget is one Pod's limit, not the group's sum.
- **A whole group is the unit of loss.** `RecreateGroupOnPodRestart` means one
  evicted worker rebuilds the group, and every model warm in it is gone. A group
  holds `size x --gpus` accelerators, so this is a far larger blast radius than a
  single-Pod pool's.

### Whether it is worth it

A group holds many more accelerators per warm unit and saves much more when used,
for far fewer models. It pays when several large models share the group and are
individually idle; it does not pay for one model, where an always-on replica
costs the same accelerators and answers requests.

`deploy/warmpool.sh sizing --params <N>B` will tell you which case you are in.
