#!/bin/sh
set -eu

chart="${REPRISE_CHART:-charts/reprise}"
helm install reprise "$chart" --namespace reprise --create-namespace \
    --values test/e2e-values.yaml --wait --timeout 3m

kubectl create configmap reprise-target --from-literal=mode=normal
cat <<'EOF' | kubectl create -f -
apiVersion: reprise.naji-dev.de/v1alpha1
kind: RepriseRequest
metadata:
  name: smoke
  namespace: default
spec:
  mutations:
    - target:
        apiVersion: v1
        kind: ConfigMap
        name: reprise-target
      patch:
        type: merge
        value:
          data:
            mode: paused
EOF

kubectl wait --for=condition=Ready repriserequest/smoke --timeout=2m
test "$(kubectl get configmap reprise-target -o jsonpath='{.data.mode}')" = paused

cat <<'EOF' | kubectl create -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: reprise-scale
spec:
  replicas: 1
  selector:
    matchLabels:
      app: reprise-scale
  template:
    metadata:
      labels:
        app: reprise-scale
    spec:
      containers:
        - name: hold
          image: registry.k8s.io/pause:3.10
          imagePullPolicy: Never
EOF

pod=""
attempt=0
while [ -z "$pod" ] && [ "$attempt" -lt 120 ]; do
    pod="$(kubectl get pods -l app=reprise-scale -o name)"
    attempt=$((attempt + 1))
    sleep 1
done
test -n "$pod"
kubectl patch "$pod" --type=merge \
    -p '{"metadata":{"finalizers":["reprise.naji-dev.de/test-hold"]}}'
cat <<'EOF' | kubectl create -f -
apiVersion: reprise.naji-dev.de/v1alpha1
kind: RepriseRequest
metadata:
  name: scale
  namespace: default
spec:
  mutations:
    - target:
        apiVersion: apps/v1
        kind: Deployment
        name: reprise-scale
      patch:
        type: merge
        value:
          spec:
            replicas: 0
EOF

kubectl wait --for=jsonpath='{.spec.replicas}'=0 deployment/reprise-scale --timeout=2m
if kubectl wait --for=condition=Ready repriserequest/scale --timeout=5s; then
    echo "scale request became Ready while its Pod still existed" >&2
    exit 1
fi
test "$(kubectl get deployment reprise-scale -o jsonpath='{.spec.replicas}')" = 0
kubectl patch "$pod" --type=merge -p '{"metadata":{"finalizers":null}}'
kubectl wait --for=condition=Ready repriserequest/scale --timeout=2m
test -z "$(kubectl get pods -l app=reprise-scale -o name)"

kubectl -n reprise rollout restart deployment/reprise-reprise
kubectl -n reprise rollout status deployment/reprise-reprise --timeout=2m
kubectl wait --for=condition=Ready repriserequest/smoke --timeout=2m
kubectl wait --for=condition=Ready repriserequest/scale --timeout=2m
kubectl delete repriserequest/smoke --wait=true --timeout=2m
kubectl delete repriserequest/scale --wait=true --timeout=2m
test "$(kubectl get configmap reprise-target -o jsonpath='{.data.mode}')" = normal
test "$(kubectl get deployment reprise-scale -o jsonpath='{.spec.replicas}')" = 1
