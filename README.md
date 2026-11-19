# tenant-operator

[![CI](https://github.com/roberto-t-plascencia/tenant-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/roberto-t-plascencia/tenant-operator/actions/workflows/ci.yml)

A Kubernetes operator, written in Go with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime), that turns a cluster-scoped `Tenant` resource into an isolated, quota-bound namespace. Teams ask for a tenant declaratively, and the controller keeps it that way.

```yaml
apiVersion: tenancy.plascencia.dev/v1alpha1
kind: Tenant
metadata:
  name: acme
spec:
  displayName: Acme Corp
  owners: [alice@acme.io, bob@acme.io]
  isolation: Strict        # or None
  quota:
    cpu: "4"
    memory: 8Gi
    pods: 50
```

```
$ kubectl get tenants
NAME   NAMESPACE     PHASE   READY   AGE
acme   tenant-acme   Ready   True    12s
```

For each tenant the operator creates and owns:

| Object | Name | Purpose |
|---|---|---|
| `Namespace` | `tenant-<name>` | The tenant's isolation boundary |
| `ResourceQuota` | `tenant-quota` | Caps CPU and memory requests and the pod count |
| `NetworkPolicy` | `tenant-isolation` | `Strict`: ingress only from pods in the same namespace |
| `RoleBinding` | `tenant-owners` | Grants the owners the built-in `admin` ClusterRole |

This is the same shape of problem I worked on when moving a SaaS product from one instance per customer to a shared multi-tenant platform: per-tenant isolation, quotas and access, provisioned repeatably and kept correct over time.

## How the controller works

The reconcile loop is level-triggered and idempotent. Each pass compares desired and observed state and only writes what differs, so a pass with nothing to do makes **zero writes**. A unit test enforces this.

1. **Finalizer first.** A finalizer is added before anything is created, so a tenant can never be deleted while its resources leak.
2. **Namespace, with ownership checks.** The namespace gets a controller owner reference to the `Tenant`. If a namespace with that name already exists and the tenant does not own it, the controller **refuses to adopt it**. It sets `Ready=False, reason=NamespaceConflict`, emits a Warning event and does not retry. Taking over someone else's namespace would be unsafe.
3. **Child objects through `CreateOrUpdate`**, each with an owner reference. Removing a field from the spec (quota, owners, isolation) deletes the matching object.
4. **Status reflects reality.** `observedGeneration`, a `phase` and a standard `Ready` condition (`metav1.Condition`). Status is patched only when it actually changes.
5. **Deletion waits.** The finalizer deletes the namespace, which garbage-collects everything inside it. It polls until the namespace is fully gone before letting the `Tenant` go.

### Drift repair

The controller watches every object it owns. If someone deletes or edits the `NetworkPolicy` or quota by hand, the owner reference maps the event back to the `Tenant` and the next reconcile puts it back. The e2e test checks this.

### Operating at scale

- **Filtered cache.** The manager caches only objects labelled `app.kubernetes.io/managed-by=tenant-operator`. Memory grows with the number of tenants, not with the size of the cluster.
- **API rate limits.** Client-side QPS and burst are configurable (`--kube-api-qps`, `--kube-api-burst`). The workqueue combines per-item exponential backoff (500 ms up to 5 min) with an overall token bucket, so a wave of failing tenants cannot flood the API server.
- **Bounded concurrency** with `--max-concurrent-reconciles`.
- **Fewer wake-ups.** A `GenerationChanged` predicate on `Tenant` ignores status-only updates, so the controller does not trigger itself.
- **High availability.** Two replicas with leader election; liveness and readiness probes.

### Observability and debuggability

- **Prometheus metrics** on `:8080/metrics`. The built-in `controller_runtime_*` and `workqueue_*` metrics (reconcile rate, errors, queue depth, latency) are joined by:
  - `tenant_operator_reconcile_duration_seconds{result}`
  - `tenant_operator_resource_changes_total{kind,operation}`: a steady write rate while no spec changes points to drift, or to another controller fighting this one.
- **Kubernetes Events** on the `Tenant` for every create, delete, conflict and error. `kubectl describe tenant acme` shows what happened.
- **Structured logs** (zap) carry the reconcile request, so you can follow one tenant through the logs.
- **Validation at admission.** A CEL rule on the CRD rejects tenant names that could not become a valid namespace name, so the problem surfaces at `kubectl apply` instead of as a failed reconcile.

## Layout

```
api/v1alpha1/          Tenant types, kubebuilder markers, deepcopy
internal/controller/   Reconciler, metrics, unit tests (controller-runtime fake client)
cmd/main.go            Manager setup: cache filter, rate limits, probes, leader election
config/                CRD, RBAC (least privilege), Deployment, kustomize overlays, sample
test/e2e/run.sh        End-to-end test against a kind cluster
```

## Running it

Requirements: Go 1.23+, Docker, [kind](https://kind.sigs.k8s.io/) and kubectl.

```sh
make test          # unit tests with the race detector
make e2e           # create a kind cluster, deploy, and run the lifecycle test
```

Against an existing cluster:

```sh
make install                 # CRD only
make run                     # run the controller locally against your kubeconfig
kubectl apply -f config/samples/tenancy_v1alpha1_tenant.yaml
kubectl get tenants -w
```

`make generate manifests` regenerates the deepcopy code, the CRD and the ClusterRole from the code markers using controller-gen. CI fails a pull request whose generated files are out of date.

## CI

Every push and pull request goes through GitHub Actions:

1. `go mod tidy` and the generated files are checked for drift.
2. `gofmt` and `go vet`.
3. Unit tests with `-race`, plus a build.
4. An **end-to-end test on kind**: build the image, deploy with kustomize, then provision a tenant, check name validation, delete a NetworkPolicy and watch it come back, check that an unowned namespace is refused, and delete the tenant through its finalizer.

## Security notes

- The ClusterRole follows least privilege. Binding the built-in `admin` role requires the `bind` verb, and it is limited to `resourceNames: [admin]`, so the operator cannot grant any other ClusterRole.
- The operator runs as non-root on a distroless image, with a read-only root filesystem and all capabilities dropped.

## License

MIT
