#!/usr/bin/env bash
# Spins up a local kind cluster with metrics-server and a demo Deployment,
# installs the ResourcePolicy CRD, and runs the controller out-of-cluster
# against it so you can watch a recommendation get computed end to end.
#
# Requires: kind, kubectl, helm, go (all local-only tools; no cloud account
# needed). Does NOT require Prometheus to be scraping in-cluster metrics --
# it configures the controller against the FakeSource-equivalent local demo
# data documented below unless PROMETHEUS_ADDRESS is set.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-kube-rightsizer-demo}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Creating kind cluster '${CLUSTER_NAME}' (if it doesn't already exist)"
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind create cluster --name "${CLUSTER_NAME}"
else
  echo "    cluster already exists, reusing it"
fi

kubectl cluster-info --context "kind-${CLUSTER_NAME}"

echo "==> Installing metrics-server (with --kubelet-insecure-tls for kind)"
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch deployment metrics-server -n kube-system --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'

echo "==> Installing the ResourcePolicy CRD"
kubectl apply -f "${ROOT_DIR}/config/crd/bases/rightsizer.slate.dev_resourcepolicies.yaml"

echo "==> Creating the demo namespace and workload"
kubectl create namespace shop --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n shop -f "${ROOT_DIR}/hack/demo-deployment.yaml"

echo "==> Applying a demo ResourcePolicy (dry-run: no gitOps target, so"
echo "    recommendations only show up in .status, no PR is opened)"
kubectl apply -n shop -f "${ROOT_DIR}/hack/demo-resourcepolicy.yaml"

echo ""
echo "==> Cluster is ready. Next steps:"
echo "    1. Run the controller out-of-cluster against this kind cluster:"
echo "         go run ./cmd/manager --prometheus-address=http://localhost:9090"
echo "    2. Watch recommendations land in status:"
echo "         kubectl get resourcepolicy -n shop default -o yaml"
echo ""
echo "    Tear down with: kind delete cluster --name ${CLUSTER_NAME}"
