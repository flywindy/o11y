---
name: verify-kubernetes-manifests
description: Use when modifying, debugging, or reviewing Kubernetes manifests, Kustomize overlays, Helm-rendered YAML, or observability infrastructure such as Grafana, Loki, Tempo, Prometheus, Alloy, or OpenTelemetry Collector. Especially use when files under k8s/infrastructure/** change and live cluster verification with kubectl is available or requested.
---

# Verify Kubernetes Manifests

## Overview

Verify Kubernetes manifest changes against both repository files and live cluster state. Prefer proving behavior through the affected in-cluster API when possible, not only checking that YAML applies.

## Workflow

1. Inspect local state before editing.
   - Run `git status --short`.
   - Read the relevant manifest, overlay, and current diff.
   - If a live cluster is available, read the current live resource with `kubectl get ... -o yaml`.

2. Make the smallest manifest change that matches the existing style.
   - Preserve unrelated user edits.
   - Keep generated or provisioned config in sync with how the workload reloads it.

3. Apply through the same entry point users run.
   - For Kustomize-based infra, use `kubectl apply -k <kustomization-dir>`.
   - For overlays, apply the overlay path that matches the requested environment.
   - If access is blocked by kubeconfig or sandbox permissions, request escalation and continue after approval.

4. Reload workloads whose config is only read at startup.
   - Restart Grafana after datasource provisioning changes.
   - Restart OpenTelemetry Collector, Prometheus, Loki, Tempo, or Alloy when their mounted ConfigMap is not hot-reloaded.
   - Use `kubectl rollout restart ...` and then `kubectl rollout status ... --timeout=120s`.

5. Verify live state.
   - Read back ConfigMaps, Deployments, Services, and Pods with `kubectl get ... -o yaml` or `kubectl get pods`.
   - Check logs with `kubectl logs` when startup, reload, or runtime behavior might fail.
   - Query the relevant service API from inside the cluster when behavior matters.

6. Report the outcome.
   - Mention the manifest files changed.
   - State exactly which `kubectl` verification commands succeeded.
   - If live verification was not possible, explain what blocked it and what local checks were completed.

## Observability Stack Checks

Use these patterns for local Grafana/Loki/Tempo/Prometheus/Collector work:

- Grafana datasource provisioning: read `configmap/grafana-datasources`, restart `deployment/grafana`, wait for rollout, and verify the datasource behavior when practical.
- Loki labels and LogQL: run `wget` from the Grafana pod, for example `kubectl exec -n infra deploy/grafana -- wget -qO- 'http://loki.infra.svc.cluster.local:3100/loki/api/v1/labels'`.
- Collector pipelines: read `configmap/otel-collector-conf`, restart the collector if needed, and inspect collector pod logs for exporter or pipeline errors.
- Prometheus: query the Prometheus HTTP API from an in-cluster pod or through port-forward when checking metrics behavior.
- Tempo: verify trace queries through Grafana or Tempo's HTTP API when a trace/log/metric correlation path is changed.

### Verifying Trace-to-Logs Integration

When changing Grafana's `tracesToLogsV2` or Loki's `derivedFields` configuration:

1. Apply the datasource ConfigMap change and restart Grafana.
2. Query Loki from the Grafana pod with a known trace ID, replacing `<namespace>` and `<trace-id>`:

   ```bash
   kubectl exec -n <namespace> deploy/grafana -- wget -qO- \
     'http://loki.<namespace>.svc.cluster.local:3100/loki/api/v1/query_range?query={service_name=~".*"}|="<trace-id>"'
   ```

3. Verify the expected log entries are returned.
4. In Grafana, open a trace span and confirm "Logs for this span" generates the expected LogQL query and returns matching Loki logs.
5. From Loki log entries, verify the `TraceID` derived field opens the matching Tempo trace.
6. For derived field matcher changes, test both supported log formats: `traceid` from the OTLP path and `traceId` from the stdout path.

## Safety Notes

- Do not assume `kubectl apply` means the system behavior changed; ConfigMap-mounted applications may need a rollout restart.
- Do not use destructive cluster operations unless explicitly requested.
- Keep verification scoped to the affected namespace and resources.
