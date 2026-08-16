#!/usr/bin/env python3
"""
add_variant.py — Add a secondary WVA variant to an existing single-stack benchmark.

Implements Topology B: one shared InferencePool/EPP fed by two Deployments,
each with its own KEDA ScaledObject at a different variantCost. The WVA
saturation solver uses variantCost to decide which variant to scale first.

The ScaledObject is what makes the variant exist as far as WVA is concerned:
WVA has no watch and no listing, and discovers a workload from the KEDA calls
its triggers cause. Identity rides in the trigger metadata — `modelID` (which
groups the two variants) and `variantCost` (which prices them). This script
therefore clones the primary's ScaledObject rather than creating one from
scratch, so the secondary inherits the scaler address, polling and behaviour
the standup chose, and differs only where it must.

Label strategy
--------------
The InferencePool created by the primary standup selects pods by:
  llm-d.ai/inferenceServing: "true"   (camelCase)
  llm-d.ai/model:            <hash>

The primary Deployment selector additionally requires:
  llm-d.ai/inference-serving: "true"  (kebab-case)

The secondary Deployment this script creates:
  - KEEPS  llm-d.ai/inferenceServing + llm-d.ai/model  → joins the pool
  - OMITS  llm-d.ai/inference-serving (kebab)           → not claimed by primary
  - ADDS   wva.llmd.ai/variant: <suffix>                → unique selector

Both ScaledObjects carry the same modelID so the WVA solver groups them.

Usage
-----
  python hack/benchmark/add_variant.py -n NAMESPACE \
      --config hack/benchmark/scenarios/guides/variants/<name>.yaml [--dry-run]

The variant config yaml declares only what differs from the primary.
Schema:

  suffix: v2                     # required; secondary name suffix
  variantCost: "5.0"             # default "5.0"
  minReplicas: 1                 # default 1
  maxReplicas: 10                # default 10
  parallelism:
    tensor: 2                    # rewrites --tensor-parallel-size
  resources:
    nvidia.com/gpu: 2            # mirrors limits + requests on GPU containers
"""

import argparse
import copy
import json
import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML is required. Install with: pip install pyyaml",
          file=sys.stderr)
    sys.exit(1)


# ---------------------------------------------------------------------------
# kubectl helpers
# ---------------------------------------------------------------------------

def kubectl(*args, stdin=None, check=True):
    cmd = ["kubectl"] + list(args)
    result = subprocess.run(cmd, capture_output=True, text=True, input=stdin)
    if check and result.returncode != 0:
        print(f"ERROR: {' '.join(cmd)}\n{result.stderr.strip()}", file=sys.stderr)
        sys.exit(1)
    return result.stdout


def kubectl_apply(obj, dry_run=False):
    payload = json.dumps(obj)
    if dry_run:
        print("---")
        print(json.dumps(obj, indent=2))
        return
    kubectl("apply", "-f", "-", stdin=payload)


def _strip_managed(obj):
    """Remove server-managed fields before re-applying as a new object."""
    meta = obj.setdefault("metadata", {})
    for field in ("resourceVersion", "uid", "generation", "creationTimestamp",
                  "managedFields", "selfLink"):
        meta.pop(field, None)
    ann = meta.get("annotations", {})
    ann.pop("kubectl.kubernetes.io/last-applied-configuration", None)
    if not ann:
        meta.pop("annotations", None)
    obj.pop("status", None)
    tmpl_meta = obj.get("spec", {}).get("template", {}).get("metadata", {})
    tmpl_meta.pop("creationTimestamp", None)
    tmpl_meta.pop("annotations", None)
    return obj


# ---------------------------------------------------------------------------
# Variant config parsing
# ---------------------------------------------------------------------------

CONFIG_DEFAULTS = {
    "variantCost": "5.0",
    "minReplicas": 1,
    "maxReplicas": 10,
}


def load_variant_config(path):
    """Load a variant override yaml, validate, and apply defaults.

    Required: `suffix`. All other keys are optional and inherit the primary's
    value if omitted (with the defaults in CONFIG_DEFAULTS for VA fields).
    """
    p = Path(path)
    if not p.is_file():
        print(f"ERROR: variant config not found: {p}", file=sys.stderr)
        sys.exit(1)
    try:
        cfg = yaml.safe_load(p.read_text()) or {}
    except yaml.YAMLError as e:
        print(f"ERROR: failed to parse {p}: {e}", file=sys.stderr)
        sys.exit(1)
    if not isinstance(cfg, dict):
        print(f"ERROR: variant config {p} must be a yaml mapping, got "
              f"{type(cfg).__name__}", file=sys.stderr)
        sys.exit(1)
    if "suffix" not in cfg or not isinstance(cfg["suffix"], str) or not cfg["suffix"]:
        print(f"ERROR: variant config {p} must set non-empty 'suffix'",
              file=sys.stderr)
        sys.exit(1)
    for k, v in CONFIG_DEFAULTS.items():
        cfg.setdefault(k, v)
    cfg["variantCost"] = str(cfg["variantCost"])
    cfg["minReplicas"] = int(cfg["minReplicas"])
    cfg["maxReplicas"] = int(cfg["maxReplicas"])
    return cfg


# ---------------------------------------------------------------------------
# Resource discovery
# ---------------------------------------------------------------------------

def find_primary_deployment(namespace):
    # The llm-d.ai/* labels live on spec.selector.matchLabels (not metadata.labels),
    # so kubectl -l filtering doesn't work — fetch all Deployments and filter here.
    out = kubectl("get", "deployment", "-n", namespace, "-o", "json")
    items = json.loads(out)["items"]

    def _is_primary(d):
        sel = d.get("spec", {}).get("selector", {}).get("matchLabels", {})
        if sel.get("llm-d.ai/inference-serving") != "true":
            return False
        if sel.get("llm-d.ai/role") != "decode":
            return False
        # Exclude secondary variants created by this script
        if "wva.llmd.ai/variant" in sel:
            return False
        return True

    primaries = [d for d in items if _is_primary(d)]
    if not primaries:
        print("ERROR: No primary decode deployment found "
              "(spec.selector: llm-d.ai/inference-serving=true,llm-d.ai/role=decode)",
              file=sys.stderr)
        sys.exit(1)
    if len(primaries) > 1:
        names = [d["metadata"]["name"] for d in primaries]
        print(f"ERROR: Multiple primary deployments found: {names}.",
              file=sys.stderr)
        sys.exit(1)
    return primaries[0]


# Trigger types that call WVA. A ScaledObject scaled by anything else (a
# prometheus trigger, say) never contacts WVA at all, so cloning it would
# produce a secondary that is never discovered — silently, since nothing errors.
WVA_TRIGGER_TYPES = ("external", "external-push")


def find_primary_scaledobject(namespace, deployment_name):
    out = kubectl("get", "scaledobject", "-n", namespace, "-o", "json")
    sos = json.loads(out)["items"]
    matching = [so for so in sos
                if so["spec"].get("scaleTargetRef", {}).get("name") == deployment_name]
    if not matching:
        print(f"ERROR: No ScaledObject found targeting deployment "
              f"'{deployment_name}' in namespace '{namespace}'.\n"
              f"       A ScaledObject is how a workload registers with WVA — "
              f"create the primary's first:\n"
              f"       make scaledobjects-apply WVA_NS={namespace} "
              f"WVA_DEFAULT_SO_NS={namespace}",
              file=sys.stderr)
        sys.exit(1)
    if len(matching) > 1:
        names = [so["metadata"]["name"] for so in matching]
        print(f"ERROR: Multiple ScaledObjects target '{deployment_name}': {names}. "
              f"Two ScaledObjects on one target is two HPAs writing the same "
              f"replica count; remove one before adding a variant.", file=sys.stderr)
        sys.exit(1)
    return matching[0]


def wva_triggers(scaledobject):
    """The triggers on a ScaledObject that call WVA."""
    return [t for t in scaledobject["spec"].get("triggers", [])
            if t.get("type") in WVA_TRIGGER_TYPES]


def scaledobject_model_id(scaledobject):
    """The modelID the primary registers under, or None if it has no WVA trigger."""
    for t in wva_triggers(scaledobject):
        model = (t.get("metadata") or {}).get("modelID")
        if model:
            return model
    return None


# ---------------------------------------------------------------------------
# Container-arg overrides
# ---------------------------------------------------------------------------

def _override_tensor_parallel(containers, tp_value):
    """Set the effective tensor-parallel size on every container.

    Two paths:
      1. Direct exec form (`args: [..., "--tensor-parallel-size", "N", ...]`
         or `--tensor-parallel-size=N`): rewrite the flag in-place.
      2. sh -c form (the modelservice chart uses
         `command: ["/bin/sh", "-c"]` with `$VLLM_TENSOR_PARALLELISM` inside
         the shell string): override the `VLLM_TENSOR_PARALLELISM` env var
         so the runtime expansion uses the secondary's value.
    """
    flag = "--tensor-parallel-size"
    target = str(tp_value)
    for c in containers:
        # Path 1: rewrite literal flag in args
        args = c.get("args")
        replaced_in_args = False
        if isinstance(args, list):
            new_args = []
            i = 0
            while i < len(args):
                a = args[i]
                if a == flag and i + 1 < len(args):
                    new_args.extend([flag, target])
                    i += 2
                    replaced_in_args = True
                elif isinstance(a, str) and a.startswith(flag + "="):
                    new_args.append(f"{flag}={target}")
                    i += 1
                    replaced_in_args = True
                else:
                    new_args.append(a)
                    i += 1
            c["args"] = new_args
        # Path 2: override the env var that sh -c expands at runtime.
        env = c.setdefault("env", [])
        replaced_in_env = False
        for e in env:
            if e.get("name") == "VLLM_TENSOR_PARALLELISM":
                e["value"] = target
                e.pop("valueFrom", None)
                replaced_in_env = True
                break
        if not replaced_in_env:
            env.append({"name": "VLLM_TENSOR_PARALLELISM", "value": target})
        # If neither path applied (no args, no shell), append the flag as
        # a fallback so an out-of-tree exec form still picks it up.
        if not replaced_in_args and not replaced_in_env:
            if not isinstance(args, list):
                args = []
                c["args"] = args
            args.extend([flag, target])


def _override_gpu_resources(containers, gpu_count):
    """Set both limits and requests of nvidia.com/gpu on every container that
    already requests a GPU. Containers without a GPU resource are untouched
    (init containers, sidecars).
    """
    target = str(gpu_count)
    for c in containers:
        res = c.get("resources") or {}
        limits = res.get("limits") or {}
        requests = res.get("requests") or {}
        already_has_gpu = (
            "nvidia.com/gpu" in limits or "nvidia.com/gpu" in requests
        )
        if not already_has_gpu:
            continue
        limits["nvidia.com/gpu"] = target
        requests["nvidia.com/gpu"] = target
        res["limits"] = limits
        res["requests"] = requests
        c["resources"] = res


def _read_tensor_parallel(containers):
    """Return the TP value seen in the first container that sets it, or None.

    Two forms supported:
      1. `--tensor-parallel-size N` (or `=N`) literal in args.
      2. `--tensor-parallel-size $VLLM_TENSOR_PARALLELISM` (or just relying on
         the env var) — resolved against the container's env. The modelservice
         chart uses sh -c with this env-var reference, so without this lookup
         _override_tensor_parallel and _read_tensor_parallel both miss the
         actual TP value.
    """
    flag = "--tensor-parallel-size"
    for c in containers:
        args = c.get("args") or []
        env = c.get("env") or []
        env_tp = next(
            (e.get("value") for e in env if e.get("name") == "VLLM_TENSOR_PARALLELISM"),
            None,
        )
        for i, a in enumerate(args):
            if a == flag and i + 1 < len(args):
                v = args[i + 1]
                if isinstance(v, str) and v.startswith("$"):
                    return env_tp
                return v
            if isinstance(a, str) and a.startswith(flag + "="):
                v = a.split("=", 1)[1]
                if v.startswith("$"):
                    return env_tp
                return v
        # No literal flag in args. The modelservice chart wraps everything in
        # `sh -c "<long shell string>"`, so the flag isn't tokenised in args
        # at all — fall back to the env var.
        if env_tp is not None:
            return env_tp
    return None


def _read_gpu_per_pod(containers):
    """Return the GPU count from the first container with one, or None."""
    for c in containers:
        res = c.get("resources") or {}
        for bucket in ("limits", "requests"):
            v = (res.get(bucket) or {}).get("nvidia.com/gpu")
            if v is not None:
                return v
    return None


def _all_containers(deployment):
    """All scrape-relevant containers in a Deployment template (main +
    initContainers). Returns a list of dicts (mutable references)."""
    spec = deployment.get("spec", {}).get("template", {}).get("spec", {})
    return list(spec.get("containers") or []) + list(spec.get("initContainers") or [])


# ---------------------------------------------------------------------------
# Object builders
# ---------------------------------------------------------------------------

def make_secondary_deployment(primary, cfg, namespace):
    sec = copy.deepcopy(primary)
    _strip_managed(sec)

    suffix = cfg["suffix"]
    primary_name = primary["metadata"]["name"]
    sec_name = f"{primary_name}-{suffix}"
    sec["metadata"]["name"] = sec_name
    sec["metadata"]["namespace"] = namespace

    spec = sec["spec"]
    spec["replicas"] = 1

    # --- pod template labels ------------------------------------------------
    tmpl_labels = spec["template"]["metadata"].setdefault("labels", {})
    # Remove kebab label so primary Deployment selector won't claim these pods
    tmpl_labels.pop("llm-d.ai/inference-serving", None)
    # Add variant discriminator
    tmpl_labels["wva.llmd.ai/variant"] = suffix
    # The primary may still carry an llm-d.ai/variant label naming ITSELF. Nothing
    # reads it (the collector keys metrics on model_name, not on a variant label),
    # but left inherited it would name the primary on the secondary's pods, so
    # drop it rather than propagate a wrong value.
    tmpl_labels.pop("llm-d.ai/variant", None)

    # --- Deployment selector ------------------------------------------------
    # Must match the pod template labels (minus kebab, plus variant).
    # Kubernetes selector is immutable after creation so get it right once.
    sel = spec["selector"]["matchLabels"]
    sel.pop("llm-d.ai/inference-serving", None)
    sel["wva.llmd.ai/variant"] = suffix
    sel.pop("llm-d.ai/variant", None)

    # --- shape overrides ----------------------------------------------------
    pod_spec = spec["template"]["spec"]
    main_containers = pod_spec.setdefault("containers", [])

    tp = (cfg.get("parallelism") or {}).get("tensor")
    if tp is not None:
        _override_tensor_parallel(main_containers, tp)

    gpu = (cfg.get("resources") or {}).get("nvidia.com/gpu")
    if gpu is not None:
        _override_gpu_resources(main_containers, gpu)

    return sec


def make_secondary_scaledobject(primary_so, sec_dep_name, cfg, namespace):
    """Clone the primary's ScaledObject onto the secondary Deployment.

    Everything the standup chose — scaler address, polling interval, cooldown,
    fallback, advanced behaviour — is inherited. Only four things change:
    the name, what it scales, its replica bounds, and its price.

    The name keeps the `-<suffix>` ending because the ScaledObject NAMES the
    variant (WVA synthesizes its internal record from the registry entry, whose
    name is the ScaledObject's), and `dump_wva_full_timeseries.py` buckets
    per-variant metrics by `variant_name.endswith("-v2")`.
    """
    primary_name = primary_so["metadata"]["name"]
    sec = copy.deepcopy(primary_so)
    _strip_managed(sec)
    # KEDA stamps these on the object it reconciles; they must not be cloned.
    for field in ("finalizers",):
        sec["metadata"].pop(field, None)
    ann = sec["metadata"].get("annotations", {})
    for field in ("scaledobject.keda.sh/transfer-hpa-ownership",
                  "autoscaling.keda.sh/paused-replicas",
                  "autoscaling.keda.sh/paused"):
        ann.pop(field, None)
    if not ann:
        sec["metadata"].pop("annotations", None)

    sec["metadata"]["name"] = f"{primary_name}-{cfg['suffix']}"
    sec["metadata"]["namespace"] = namespace
    # Inherit controller-instance label so a namespace-scoped controller with an
    # instance configured still counts this variant as part of its fleet.
    sec["metadata"].setdefault("labels", {})
    sec["metadata"]["labels"]["wva.llmd.ai/controller-instance"] = namespace

    spec = sec["spec"]
    spec["scaleTargetRef"]["name"] = sec_dep_name
    spec["minReplicaCount"] = cfg["minReplicas"]
    spec["maxReplicaCount"] = cfg["maxReplicas"]

    for t in wva_triggers(sec):
        meta = t.setdefault("metadata", {})
        meta["variantCost"] = cfg["variantCost"]
        # Nothing else needs stripping. The scale target is read from each
        # ScaledObject's own scaleTargetRef and cannot be named in metadata, so a
        # clone cannot point the secondary's registry entry at the primary's
        # Deployment. That used to require popping an inherited "variantName";
        # the key was removed from WVA precisely because it could disagree.

    return sec


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(
        description="Add a secondary WVA variant to an existing benchmark deployment."
    )
    ap.add_argument("-n", "--namespace", required=True,
                    help="Kubernetes namespace")
    ap.add_argument("--config", required=True,
                    help="Path to a variant override yaml (see module docstring)")
    ap.add_argument("--dry-run", action="store_true",
                    help="Print manifests as JSON without applying")
    args = ap.parse_args()

    ns = args.namespace
    cfg = load_variant_config(args.config)
    suffix = cfg["suffix"]

    print(f"[1/2] Finding primary decode Deployment in namespace '{ns}'...")
    primary_dep = find_primary_deployment(ns)
    dep_name = primary_dep["metadata"]["name"]
    model_hash = (primary_dep.get("spec", {}).get("selector", {})
                  .get("matchLabels", {}).get("llm-d.ai/model", "?"))
    primary_containers = _all_containers(primary_dep)
    primary_tp = _read_tensor_parallel(primary_containers) or "1"
    primary_gpu = _read_gpu_per_pod(primary_containers) or "1"
    print(f"      {dep_name}  (llm-d.ai/model={model_hash})")

    print(f"[2/2] Finding the primary's ScaledObject...")
    primary_so = find_primary_scaledobject(ns, dep_name)
    if not wva_triggers(primary_so):
        types = [t.get("type") for t in primary_so["spec"].get("triggers", [])]
        print(f"ERROR: ScaledObject '{primary_so['metadata']['name']}' has no WVA "
              f"trigger (found: {types or 'none'}).\n"
              f"       Only {'/'.join(WVA_TRIGGER_TYPES)} triggers call WVA. A "
              f"secondary cloned from this one would never be discovered, and "
              f"nothing would report that — it would simply sit at its replica "
              f"count. Point the primary at WVA first:\n"
              f"       make scaledobjects-apply WVA_NS={ns} WVA_DEFAULT_SO_NS={ns} "
              f"WVA_DEFAULT_SO_ADOPT=true", file=sys.stderr)
        sys.exit(1)
    model_id = scaledobject_model_id(primary_so)
    if not model_id:
        print(f"ERROR: ScaledObject '{primary_so['metadata']['name']}' declares no "
              f"modelID in its trigger metadata. modelID is what groups the two "
              f"variants; without it there is nothing for the solver to compare "
              f"them under.", file=sys.stderr)
        sys.exit(1)
    primary_cost = next(
        ((t.get("metadata") or {}).get("variantCost") for t in wva_triggers(primary_so)
         if (t.get("metadata") or {}).get("variantCost")),
        "10.0 (default)",
    )
    print(f"      {primary_so['metadata']['name']}  "
          f"(modelID={model_id}, variantCost={primary_cost})")

    sec_dep_name = f"{dep_name}-{suffix}"

    print(f"\nCreating secondary variant '{suffix}'  "
          f"variantCost={cfg['variantCost']}  modelID={model_id}\n")

    sec_dep = make_secondary_deployment(primary_dep, cfg, ns)
    sec_so = make_secondary_scaledobject(primary_so, sec_dep_name, cfg, ns)

    print(f"  Applying Deployment: {sec_dep_name}")
    kubectl_apply(sec_dep, dry_run=args.dry_run)

    # The owner ref on the ScaledObject points at the secondary Deployment so
    # `make benchmark-teardown` (which deletes the Deployment via the helm
    # release's cascade) garbage-collects it too. Without this it orphans in the
    # namespace, and an orphaned ScaledObject keeps registering a workload that
    # no longer exists.
    if not args.dry_run:
        sec_dep_uid = json.loads(kubectl(
            "get", "deployment", sec_dep_name, "-n", ns, "-o", "json",
        ))["metadata"]["uid"]
        owner_ref = {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "name": sec_dep_name,
            "uid": sec_dep_uid,
            "blockOwnerDeletion": True,
            "controller": False,
        }
        sec_so.setdefault("metadata", {}).setdefault("ownerReferences", []).append(owner_ref)

    print(f"  Applying ScaledObject: {sec_so['metadata']['name']}")
    kubectl_apply(sec_so, dry_run=args.dry_run)

    if args.dry_run:
        return

    sec_containers = _all_containers(sec_dep)
    sec_tp = _read_tensor_parallel(sec_containers) or "1"
    sec_gpu = _read_gpu_per_pod(sec_containers) or "1"

    print()
    print("Secondary variant created successfully.")
    print(f"  Primary   (cost {primary_cost:>5}, TP={primary_tp}, "
          f"{primary_gpu} GPU/pod): {dep_name}")
    print(f"  Secondary (cost {cfg['variantCost']:>5}, TP={sec_tp}, "
          f"{sec_gpu} GPU/pod): {sec_dep_name}")
    print()
    print("Both ScaledObjects carry modelID=" + repr(model_id) + ".")
    print("WVA will scale the more efficient variant first (most served load")
    print("per unit cost), spilling over to the other only once it saturates.")
    print()
    print("Verify:")
    print(f"  kubectl get scaledobject,hpa -n {ns}")
    print(f"  kubectl get pods -n {ns} "
          f"-l 'llm-d.ai/inferenceServing=true,llm-d.ai/model={model_hash}'")
    print()
    print("The secondary is not a variant until KEDA has called WVA about it.")
    print("Until then it is a Deployment WVA has never heard of:")
    print(f"  kubectl get scaledobject {sec_so['metadata']['name']} -n {ns} "
          f"-o jsonpath='{{.status.conditions}}'")


if __name__ == "__main__":
    main()
