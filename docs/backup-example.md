# Backup orchestration example

The workflow decides what to patch. Reprise treats the HelmRelease and
Deployment as generic Kubernetes targets. The controller does not run a
backup or interact with Rustic.

Create a `RepriseRequest` whose mutations are ordered so reconciliation is
suspended before scaling the workload:

```yaml
apiVersion: reprise.naji-dev.de/v1alpha1
kind: RepriseRequest
metadata:
  name: vaultwarden-backup
  namespace: vaultwarden
spec:
  mutations:
    - target:
        apiVersion: helm.toolkit.fluxcd.io/v2
        kind: HelmRelease
        name: vaultwarden
      patch:
        type: merge
        value:
          spec:
            suspend: true
    - target:
        apiVersion: apps/v1
        kind: Deployment
        name: vaultwarden
      patch:
        type: merge
        value:
          spec:
            replicas: 0
```

An external orchestrator can create that request before starting its backup
job. Always arrange deletion, even when waiting or backup fails:

```sh
#!/bin/sh
set -eu

request=vaultwarden-backup
cleanup() {
    kubectl -n vaultwarden delete "repriserequest/$request" --wait=true
}
trap cleanup EXIT

kubectl create -f request.yaml
kubectl -n vaultwarden wait --for=condition=Ready \
    "repriserequest/$request" --timeout=10m
rustic backup /data
```

For a Kubernetes Job, create the request in an init container and set an
owner reference to the Job. Keep the Job until the main container is terminal,
then delete the Job to trigger garbage collection of the request. Wait for
the request's Ready condition before starting the backup. The Job must have
a finite deadline and a cleanup path, or an active request can remain forever.

This example assumes a Deployment exists and that the controller has explicit
`get`/`patch` permissions on HelmReleases and Deployments. Flux may already
be reconciling at the moment it is suspended; Reprise verifies the resulting
target fields before reporting Ready.
