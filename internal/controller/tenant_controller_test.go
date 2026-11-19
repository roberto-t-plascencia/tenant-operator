package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tenancyv1alpha1 "github.com/roberto-t-plascencia/tenant-operator/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := tenancyv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newReconciler(t *testing.T, objs ...client.Object) (*TenantReconciler, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&tenancyv1alpha1.Tenant{}).
		Build()
	return &TenantReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100)}, c
}

func tenant(name string) *tenancyv1alpha1.Tenant {
	cpu := resource.MustParse("2")
	mem := resource.MustParse("4Gi")
	pods := int32(20)
	return &tenancyv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: tenancyv1alpha1.TenantSpec{
			DisplayName: "Acme Corp",
			Owners:      []string{"bob@acme.io", "alice@acme.io", "bob@acme.io"},
			Quota:       &tenancyv1alpha1.TenantQuota{CPU: &cpu, Memory: &mem, Pods: &pods},
			Isolation:   tenancyv1alpha1.IsolationStrict,
		},
	}
}

func reconcileOnce(t *testing.T, r *TenantReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getTenant(t *testing.T, c client.Client, name string) *tenancyv1alpha1.Tenant {
	t.Helper()
	got := &tenancyv1alpha1.Tenant{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	return got
}

func TestReconcileProvisionsTenant(t *testing.T) {
	r, c := newReconciler(t, tenant("acme"))
	reconcileOnce(t, r, "acme")
	ctx := context.Background()

	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-acme"}, ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if ns.Labels[ManagedByLabel] != ManagedByValue || ns.Labels[TenantLabel] != "acme" {
		t.Errorf("namespace labels = %v", ns.Labels)
	}
	if ref := metav1.GetControllerOf(ns); ref == nil || ref.Name != "acme" || ref.Kind != "Tenant" {
		t.Errorf("namespace controller ref = %+v", ref)
	}

	rq := &corev1.ResourceQuota{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: QuotaName}, rq); err != nil {
		t.Fatalf("quota not created: %v", err)
	}
	if q := rq.Spec.Hard[corev1.ResourceRequestsCPU]; q.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu quota = %s", q.String())
	}
	if q := rq.Spec.Hard[corev1.ResourcePods]; q.Value() != 20 {
		t.Errorf("pods quota = %s", q.String())
	}

	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: NetworkPolicyName}, np); err != nil {
		t.Fatalf("network policy not created: %v", err)
	}

	rb := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: OwnersBindingName}, rb); err != nil {
		t.Fatalf("role binding not created: %v", err)
	}
	if len(rb.Subjects) != 2 || rb.Subjects[0].Name != "alice@acme.io" || rb.Subjects[1].Name != "bob@acme.io" {
		t.Errorf("subjects should be sorted and de-duplicated, got %+v", rb.Subjects)
	}
	if rb.RoleRef.Name != "admin" {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}

	got := getTenant(t, c, "acme")
	if got.Status.Phase != tenancyv1alpha1.PhaseReady || got.Status.Namespace != "tenant-acme" {
		t.Errorf("status = %+v", got.Status)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady) {
		t.Errorf("Ready condition not true: %+v", got.Status.Conditions)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, generation = %d", got.Status.ObservedGeneration, got.Generation)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Errorf("finalizers = %v", got.Finalizers)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	r, c := newReconciler(t, tenant("acme"))
	reconcileOnce(t, r, "acme")
	before := getTenant(t, c, "acme").ResourceVersion

	rq := &corev1.ResourceQuota{}
	key := types.NamespacedName{Namespace: "tenant-acme", Name: QuotaName}
	if err := c.Get(context.Background(), key, rq); err != nil {
		t.Fatal(err)
	}
	quotaBefore := rq.ResourceVersion

	reconcileOnce(t, r, "acme")

	if after := getTenant(t, c, "acme").ResourceVersion; after != before {
		t.Errorf("tenant was written on a no-op reconcile (%s -> %s)", before, after)
	}
	if err := c.Get(context.Background(), key, rq); err != nil {
		t.Fatal(err)
	}
	if rq.ResourceVersion != quotaBefore {
		t.Errorf("quota was written on a no-op reconcile (%s -> %s)", quotaBefore, rq.ResourceVersion)
	}
}

func TestReconcileRepairsDrift(t *testing.T) {
	r, c := newReconciler(t, tenant("acme"))
	reconcileOnce(t, r, "acme")
	ctx := context.Background()

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: NetworkPolicyName}}
	if err := c.Delete(ctx, np); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "acme")
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), np); err != nil {
		t.Errorf("deleted network policy was not recreated: %v", err)
	}
}

func TestReconcileAppliesSpecChanges(t *testing.T) {
	r, c := newReconciler(t, tenant("acme"))
	reconcileOnce(t, r, "acme")
	ctx := context.Background()

	got := getTenant(t, c, "acme")
	got.Spec.Quota = nil
	got.Spec.Owners = nil
	got.Spec.Isolation = tenancyv1alpha1.IsolationNone
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "acme")

	for _, obj := range []client.Object{
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: QuotaName}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: NetworkPolicyName}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: OwnersBindingName}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T should have been removed, got err=%v", obj, err)
		}
	}
}

func TestReconcileRefusesUnownedNamespace(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}
	r, c := newReconciler(t, tenant("acme"), existing)
	reconcileOnce(t, r, "acme")

	got := getTenant(t, c, "acme")
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNamespaceConflict {
		t.Fatalf("expected NamespaceConflict condition, got %+v", cond)
	}
	if got.Status.Phase != tenancyv1alpha1.PhaseFailed {
		t.Errorf("phase = %s", got.Status.Phase)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tenant-acme"}, ns); err != nil {
		t.Fatal(err)
	}
	if len(ns.OwnerReferences) != 0 || ns.Labels[ManagedByLabel] != "" {
		t.Errorf("unowned namespace was modified: %+v", ns.ObjectMeta)
	}
}

func TestDeletionWaitsForNamespace(t *testing.T) {
	r, c := newReconciler(t, tenant("acme"))
	reconcileOnce(t, r, "acme")
	ctx := context.Background()

	if err := c.Delete(ctx, getTenant(t, c, "acme")); err != nil {
		t.Fatal(err)
	}

	// First pass deletes the namespace and polls instead of releasing the finalizer.
	if res := reconcileOnce(t, r, "acme"); res.RequeueAfter == 0 {
		t.Errorf("expected a requeue while the namespace is deleted, got %+v", res)
	}
	if got := getTenant(t, c, "acme"); got.Status.Phase != tenancyv1alpha1.PhaseTerminating {
		t.Errorf("phase = %s", got.Status.Phase)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-acme"}, ns); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace should be deleted, err=%v", err)
	}

	// Second pass sees the namespace gone and releases the finalizer.
	reconcileOnce(t, r, "acme")
	err := c.Get(ctx, types.NamespacedName{Name: "acme"}, &tenancyv1alpha1.Tenant{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("tenant should be gone after the finalizer is released, err=%v", err)
	}
}

func TestReconcileMissingTenantIsNoop(t *testing.T) {
	r, _ := newReconciler(t)
	reconcileOnce(t, r, "does-not-exist")
}
