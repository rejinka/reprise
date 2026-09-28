# Reprise project context

Reprise is a Kubernetes controller for temporary, generic resource mutations.
Its only job is to capture affected fields, lock target resources, apply merge
patches, verify them, and restore those fields when a `RepriseRequest` is
deleted. It does not schedule work, understand Flux, or perform backups.

The API group is `reprise.naji-dev.de/v1alpha1`. Requests are namespaced, the
controller watches all namespaces, and targets may be namespaced or cluster
scoped. A missing target namespace defaults to the request namespace for a
namespaced kind. One controller installation is supported per cluster; replicas
of that installation use leader election.

Recovery state lives in the request status before any target mutation. Target
UIDs, field presence (including explicit null), original and expected values,
and scale-to-zero selectors are persisted. Targets are locked with annotations.
The request finalizer remains if restoration encounters a changed field,
replacement UID, or missing permission. Never force-remove a finalizer without
inspecting and recovering each target.

Only `merge` patches are public API. JSON Patch is used internally for
optimistic locking and exact restoration. Secrets are forbidden as targets.
The chart grants no target patch rights unless `targetRules` is configured.
Only trusted identities should be allowed to create or read requests because
status contains original values. Do not install two independent releases in a
cluster.

Image tags (`image/vX.Y.Z`) and chart tags (`chart/vA.B.C`) release separately.
The chart pins an image tag. Its templated CRD can be upgraded with the chart;
`crds.keep` prevents CRD removal on uninstall. The standalone CRD file is
also a release asset. Image-only releases must remain compatible with the
latest released chart and CRD.

This repository is the standalone source for Reprise. Homelab deployments and
backup orchestration live in their own repositories and are not part of the
controller or its release process.
