# Add WVA to a running llm-d

## Overview

llm-d is already deployed and serving. This adds the autoscaler to it: the
cluster's Prometheus, KEDA and CRDs are used as they are, and nothing about the
existing model servers changes.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `llm-d-optimized-baseline` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `http://prom.monitoring.svc:9090` |

Full list: [Configuration reference](../../deployment/configuration.md).

## Prerequisites

- KEDA on the cluster
- the Prometheus that already scrapes your model servers
- a cluster admin has run [`make setup-prereqs`](../admin-cluster-setup/README.md)
  for the namespace

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=llm-d-optimized-baseline
```
<!-- guide:env.static.namespace end -->

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs
```
<!-- guide:prerequisites.check end -->

Confirm the Prometheus it reports is the one scraping your model servers. If it
is not, pass `PROMETHEUS_URL`.

## Installation

<!-- guide:deploy.controller start -->
```bash
make deploy-wva INSTALL_PHASE=wva
```
<!-- guide:deploy.controller end -->

### Register the workloads

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=/tmp/wva-scaledobject-plan.XXXX
```
<!-- guide:deploy.register end -->

Workloads something else already scales keep their ScaledObject unless you pass
`WVA_DEFAULT_SO_ADOPT=true`, which repoints the triggers at WVA. Two ScaledObjects
on one target is two HPAs writing the same replica count.

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

## Next

- [After the install](../../deployment/operations.md)
- [Bounding GPU usage](../admin-gpu-bounding/README.md)
