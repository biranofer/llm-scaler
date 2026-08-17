# Workload-Variant-Autoscaler Documentation

Welcome to the WVA documentation! This directory contains comprehensive guides for users, developers, and operators.

## Documentation Structure

### User Guide


- **[Guides](guides/README.md)** - Installing WVA, and the tasks you do after
- **[Configuration](deployment/configuration.md)** - Every variable the installer reads
- **[Scaling policy configuration](developer-guide/scaling-policy-config.md)** - Thresholds, tiers, scale-to-zero, limiters
- **[After the install](deployment/operations.md)** - What to watch, first-line troubleshooting
- **[Architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)** - Where WVA sits among llm-d's autoscaling paths

### Design

- **[Modeling & Optimization](design/modeling-optimization.md)** - Queue theory models and optimization algorithms
- **[Controller Behavior](design/controller-behavior.md)** - Event handling and reconciliation behavior (outdated)
- **[External scaler design](design/wva-external-scaler-proposal.md)** - How WVA drives KEDA, and why
- **[Unified Configuration System](developer-guide/configuration.md)** - Configuration reference for all WVA components
- **[Metrics & Health Monitoring](developer-guide/metrics-health-monitoring.md)** - Exposed metrics and health check endpoints
- **[Saturation Scaling Configuration](developer-guide/scaling-policy-config.md)** - Tuning the saturation-based scaling algorithm
- **[Quota Limiter](developer-guide/quota-limiter.md)** - Operator-declared per-accelerator GPU caps (cluster/namespace scope)
- **[GPU Capacity Accounting](developer-guide/gpu-capacity-accounting.md)** - What the GPU budget means, and the three known ways it over-states free capacity
- **[Throughput Analyzer](developer-guide/throughput-analyzer.md)** - How the throughput analyzer works
- **[Queue Model Analyzer](developer-guide/slo-queuemodel.md)** - SLO-aware queueing model
- **[Pod Scraping Source](developer-guide/pod-scraping-source.md)** - Direct pod metric scraping
- **[Prometheus Integration](developer-guide/prometheus.md)** - Prometheus metrics and configuration
- **[WVA with Fast Model Actuation](guides/fma/README.md)** - Running WVA in a namespace that uses FMA: scraping the launchers, what the plan targets, sizing from the launcher pool
- **[FMA-aware attribution](proposals/fma-aware-attribution.md)** - How WVA measures a Fast Model Actuation variant, whose engine runs in a pod no ScaledObject owns
- **[Requests to Fast Model Actuation](proposals/fma-upstream-requests.md)** - Findings and change requests for the FMA project

### Developer Guide

- **[Development Setup](developer-guide/development.md)** - Setting up your dev environment
- **[Testing](developer-guide/testing.md)** - Running tests and CI workflows
- **[Debugging](developer-guide/debugging.md)** - Debugging techniques and tools
- **[Contributing](../CONTRIBUTING.md)** - How to contribute to the project

### Benchmark Guide

- **[Benchmark Guide](developer-guide/benchmark-guide.md)** - Running WVA scaling benchmarks

## Quick Links

- [Main README](../README.md)
- [Kubernetes Deployment](../deploy/kubernetes/README.md)
- [OpenShift Deployment](../deploy/openshift/README.md)
- [Local Development with Kind Emulator](../deploy/kind-emulator/README.md)


## Need Help?

- Check the [Troubleshooting Guide](developer-guide/troubleshooting.md)
- Open a [GitHub Issue](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
- Join community meetings
