# Reprise

Reprise temporarily changes Kubernetes resources and restores the affected
fields when a `RepriseRequest` is deleted. It can be used by backup
orchestrators, maintenance scripts, and other short-lived workflows. Reprise
does not run those workflows or have any Flux-specific behavior.

The Go module is `github.com/rejinka/reprise`. Typed request definitions are
available from `github.com/rejinka/reprise/v1alpha1`; mutation targets remain
dynamic Kubernetes objects.

## Install

Install the chart once per cluster. It installs the CRD by default and keeps
the CRD on uninstall. Grant only the target permissions your requests need:

```yaml
targetRules:
  - apiGroups:
      - helm.toolkit.fluxcd.io
    resources:
      - helmreleases
    verbs:
      - get
      - patch
  - apiGroups:
      - apps
    resources:
      - deployments
    verbs:
      - get
      - patch
```

```sh
helm install reprise oci://ghcr.io/rejinka/charts/reprise \
  --namespace reprise --create-namespace --values values.yaml
```

The controller watches requests in all namespaces. Only trusted service
accounts should have permission to create or read them: the status contains
original field values. Reprise refuses to target Kubernetes Secrets. A
cluster-scoped target has no namespace; a namespaced target without one uses
the request namespace.

`targetRules` grants rights in every namespace because the chart places each
rule in a ClusterRole. To limit a target permission to selected namespaces,
leave it out of `targetRules` and bind a separate Role to the Reprise service
account in each allowed namespace. Grant cluster-scoped target rights through
a separate ClusterRoleBinding when needed.

## Use

Create a request, wait for its `Ready` condition, do the work, then delete it
with `--wait=true`. Deletion waits for the finalizer to restore fields:

```sh
kubectl create -f request.yaml
kubectl -n example wait --for=condition=Ready repriserequest/example --timeout=10m
# Run the workflow that needs the temporary state.
kubectl -n example delete repriserequest/example --wait=true
```

See [the request example](config/samples/request.yaml) and
[the backup orchestration example](docs/backup-example.md). A request that
never gets deleted remains active; Reprise has no TTL. The workflow must
arrange deletion, including on failure. `Ready=True` is valid only when its
condition `observedGeneration` matches the current request generation.

## Behavior

Reprise records original values in request status before it changes a target.
It locks all targets before applying mutations. A second request targeting
any locked resource remains non-ready. Requests cannot change `spec` after
creation. A patch to `spec.replicas: 0` on a Deployment, StatefulSet, or
standalone ReplicaSet becomes ready only after its controller observed the
new generation and matching Pods have disappeared. Other patches are verified
by reading back the affected fields. Restoring a request touches only fields
recorded for its mutation; changed affected fields produce a conflict
condition and keep the finalizer. A target recreated with the same name has
a new UID and is never patched as the old target.

Merge Patch replaces arrays as a whole, so an array mutation locks that whole
array for conflict checking. If an originally absent parent object must be
created for a patch, that parent is the affected field. Prefer patching
existing object trees when unrelated writers may modify them concurrently.

The controller needs `get` and `patch` permission on each allowed target,
plus `list` for Pods when scale-to-zero readiness is used. The chart only
grants request, leader-election, and optional Pod-read permissions
by default. Configure target permissions explicitly. `kubectl describe`
and the Ready condition explain missing permissions or restoration conflicts.

## Development

Run `make test`, `make lint`, and `make chart-check`. The GitHub Actions
workflows include a Kind scenario for creation, restoration, and a controller
restart. Image and chart releases are triggered by independent tags
`image/vX.Y.Z` and `chart/vA.B.C`.

Licensed under Apache-2.0.
