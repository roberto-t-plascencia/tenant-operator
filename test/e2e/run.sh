#!/usr/bin/env bash
# End-to-end test: deploys the operator to a kind cluster and walks a Tenant
# through its lifecycle: provision, validation, drift repair, conflict, deletion.
set -euo pipefail
cd "$(dirname "$0")/../.."

IMG=ghcr.io/roberto-t-plascencia/tenant-operator:e2e
CLUSTER=${KIND_CLUSTER:-kind}
NS=tenant-operator-system

step() { echo; echo "==> $*"; }
fail() { echo "FAIL: $*" >&2; dump; exit 1; }
dump() {
  kubectl -n "$NS" logs deploy/tenant-operator-controller-manager --tail=100 || true
  kubectl get tenants -o wide || true
  kubectl get events -A --sort-by=.lastTimestamp | tail -30 || true
}

step "Build and load image"
docker build -t "$IMG" .
kind load docker-image "$IMG" --name "$CLUSTER"

step "Deploy operator"
kubectl apply -k test/e2e
kubectl -n "$NS" rollout status deploy/tenant-operator-controller-manager --timeout=180s

step "Provision a tenant"
kubectl apply -f config/samples/tenancy_v1alpha1_tenant.yaml
kubectl wait tenant/acme --for=condition=Ready --timeout=120s || fail "tenant never became Ready"
kubectl get tenant acme
kubectl -n tenant-acme get resourcequota/tenant-quota networkpolicy/tenant-isolation rolebinding/tenant-owners \
  || fail "tenant resources missing"
[ "$(kubectl -n tenant-acme get resourcequota tenant-quota -o jsonpath='{.spec.hard.pods}')" = "50" ] \
  || fail "quota not applied"

step "Reject a name that cannot become a namespace"
if kubectl apply -f - <<YAML 2>/dev/null
apiVersion: tenancy.plascencia.dev/v1alpha1
kind: Tenant
metadata:
  name: this-tenant-name-is-much-too-long-to-become-a-namespace-name
YAML
then fail "invalid tenant name was accepted"; fi
echo "rejected as expected"

step "Repair drift: deleted NetworkPolicy is recreated"
kubectl -n tenant-acme delete networkpolicy tenant-isolation
ok=""
for _ in $(seq 30); do
  if kubectl -n tenant-acme get networkpolicy tenant-isolation >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ -n "$ok" ] || fail "network policy was not recreated"
echo "recreated"

step "Refuse to adopt a namespace the operator does not own"
kubectl create namespace tenant-taken
kubectl apply -f - <<YAML
apiVersion: tenancy.plascencia.dev/v1alpha1
kind: Tenant
metadata:
  name: taken
YAML
kubectl wait tenant/taken --for=jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'=NamespaceConflict --timeout=60s \
  || fail "conflict not reported"
kubectl delete tenant taken --timeout=60s
kubectl get namespace tenant-taken >/dev/null || fail "unowned namespace was deleted"

step "Delete the tenant; finalizer waits for its namespace"
kubectl delete tenant acme --timeout=180s
kubectl get namespace tenant-acme >/dev/null 2>&1 && fail "namespace still exists after tenant deletion"

step "Metrics"
kubectl -n "$NS" port-forward deploy/tenant-operator-controller-manager 18080:8080 >/dev/null 2>&1 &
pf=$!
sleep 3
curl -s localhost:18080/metrics | grep -E '^tenant_operator_resource_changes_total' || true
kill $pf || true

echo
echo "PASS"
