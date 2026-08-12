#!/usr/bin/env bash
#
# Shared environment for the WVA guides. Source it before running guide commands:
#
#     source docs/guides/env.sh
#
# Same shape as llm-d's guides/env.sh: versions and image references live here,
# because they change together and no guide should carry its own copy. The
# NAMESPACE deliberately does not — each guide exports the one it installs into,
# exactly as llm-d's guides do.

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
export REPO_ROOT

# The controller image the guides install.
#
# Set this to a build of your own tree when you are testing an unmerged branch:
# the default is a published image, and a released image does not accept flags a
# newer branch's manifests pass. That mismatch presents as CrashLoopBackOff,
# which reads like a broken image rather than a version skew.
export WVA_IMAGE="${WVA_IMAGE:-ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest}"

# The namespace WVA installs into.
#
# Left unset on purpose. The install uses WVA_NS, then NAMESPACE — llm-d's own
# variable — and if neither is set it FINDS the namespace running llm-d model
# servers. So a reader who has followed an llm-d guide already has this right,
# and everyone else can leave it alone and let the preflight report what it found.
#
#   export NAMESPACE=llm-d-optimized-baseline

# KEDA and Prometheus are prerequisites rather than variables: the install adds
# KEDA if the cluster has none, and detects Prometheus (on OpenShift, the
# platform's Thanos Querier at a fixed address). PROMETHEUS_URL overrides that.
