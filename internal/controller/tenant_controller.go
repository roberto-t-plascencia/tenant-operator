// Package controller implements the Tenant reconciler.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/time/rate"

	tenancyv1alpha1 "github.com/roberto-t-plascencia/tenant-operator/api/v1alpha1"
)

const (
	// Finalizer guards tenant deletion until the tenant namespace is gone.
	Finalizer = "tenancy.plascencia.dev/finalizer"
	// TenantLabel records which tenant a managed object belongs to.
	TenantLabel = "tenancy.plascencia.dev/tenant"
	// ManagedByLabel and ManagedByValue mark every object this operator owns.
	// The manager's cache is filtered on this label so memory scales with
	// managed objects, not with everything in the cluster.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "tenant-operator"

	// NamespacePrefix is prepended to the tenant name to form its namespace.
	NamespacePrefix = "tenant-"

	QuotaName         = "tenant-quota"
	NetworkPolicyName = "tenant-isolation"
	OwnersBindingName = "tenant-owners"

	// ConditionReady reports whether every resource for the tenant is in place.
	ConditionReady = "Ready"

	ReasonProvisioned       = "Provisioned"
	ReasonNamespaceConflict = "NamespaceConflict"
	ReasonReconcileError    = "ReconcileError"
	ReasonTerminating       = "Terminating"

	// namespaceDeletePoll is how often a deleting tenant re-checks its namespace.
	namespaceDeletePoll = 5 * time.Second
)

// NamespaceFor returns the namespace name provisioned for a tenant.
func NamespaceFor(t *tenancyv1alpha1.Tenant) string { return NamespacePrefix + t.Name }

// conflictError means the tenant's namespace exists but this operator does not own it.
// It is not retried: adopting someone else's namespace would be unsafe.
type conflictError struct{ namespace string }

func (e *conflictError) Error() string {
	return fmt.Sprintf("namespace %q already exists and is not owned by this tenant", e.namespace)
}

// TenantReconciler provisions an isolated, quota-bound namespace for each Tenant.
type TenantReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// MaxConcurrentReconciles bounds parallel reconciles (default 1).
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=tenancy.plascencia.dev,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=tenancy.plascencia.dev,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tenancy.plascencia.dev,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=admin
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one Tenant toward its desired state. It is idempotent:
// running it again with nothing changed makes no writes.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	start := time.Now()
	defer func() { observeReconcile(start, err) }()

	tenant := &tenancyv1alpha1.Tenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tenant.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, tenant)
	}

	if controllerutil.AddFinalizer(tenant, Finalizer) {
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
	}

	if tenant.Status.Phase == "" {
		if err := r.setStatus(ctx, tenant, tenancyv1alpha1.PhaseProvisioning, metav1.ConditionFalse,
			"Provisioning", "provisioning tenant resources"); err != nil {
			return ctrl.Result{}, err
		}
	}

	ns := NamespaceFor(tenant)
	if err := r.ensureNamespace(ctx, tenant, ns); err != nil {
		var conflict *conflictError
		if errors.As(err, &conflict) {
			r.Recorder.Event(tenant, corev1.EventTypeWarning, ReasonNamespaceConflict, conflict.Error())
			return ctrl.Result{}, r.setStatus(ctx, tenant, tenancyv1alpha1.PhaseFailed,
				metav1.ConditionFalse, ReasonNamespaceConflict, conflict.Error())
		}
		return r.fail(ctx, tenant, fmt.Errorf("namespace: %w", err))
	}

	steps := []struct {
		name string
		fn   func(context.Context, *tenancyv1alpha1.Tenant, string) error
	}{
		{"resource quota", r.ensureQuota},
		{"network policy", r.ensureNetworkPolicy},
		{"owner role binding", r.ensureOwnerBinding},
	}
	for _, s := range steps {
		if err := s.fn(ctx, tenant, ns); err != nil {
			return r.fail(ctx, tenant, fmt.Errorf("%s: %w", s.name, err))
		}
	}

	return ctrl.Result{}, r.setStatus(ctx, tenant, tenancyv1alpha1.PhaseReady, metav1.ConditionTrue,
		ReasonProvisioned, fmt.Sprintf("namespace %s is provisioned", ns))
}

// fail records a retryable error on the tenant and returns it so the
// workqueue requeues the request with exponential backoff.
func (r *TenantReconciler) fail(ctx context.Context, t *tenancyv1alpha1.Tenant, cause error) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "reconcile failed")
	r.Recorder.Event(t, corev1.EventTypeWarning, ReasonReconcileError, cause.Error())
	statusErr := r.setStatus(ctx, t, tenancyv1alpha1.PhaseFailed, metav1.ConditionFalse, ReasonReconcileError, cause.Error())
	return ctrl.Result{}, errors.Join(cause, statusErr)
}

// setStatus patches status only when it actually changed, to avoid needless
// writes (and watch events) against the API server.
func (r *TenantReconciler) setStatus(ctx context.Context, t *tenancyv1alpha1.Tenant, phase tenancyv1alpha1.TenantPhase,
	status metav1.ConditionStatus, reason, message string) error {
	before := t.DeepCopy()
	t.Status.ObservedGeneration = t.Generation
	t.Status.Namespace = NamespaceFor(t)
	t.Status.Phase = phase
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: t.Generation,
	})
	if equality.Semantic.DeepEqual(before.Status, t.Status) {
		return nil
	}
	return r.Status().Patch(ctx, t, client.MergeFrom(before))
}

func (r *TenantReconciler) ensureNamespace(ctx context.Context, t *tenancyv1alpha1.Tenant, name string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labelsFor(t)}}
		if err := controllerutil.SetControllerReference(t, ns, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, ns); err != nil {
			// The cache only sees namespaces this operator labels, so an
			// unmanaged namespace with the same name shows up here.
			if apierrors.IsAlreadyExists(err) {
				return &conflictError{namespace: name}
			}
			return err
		}
		recordChange("Namespace", controllerutil.OperationResultCreated)
		r.Recorder.Eventf(t, corev1.EventTypeNormal, "NamespaceCreated", "created namespace %s", name)
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(ns, t) {
		return &conflictError{namespace: name}
	}
	if !ns.DeletionTimestamp.IsZero() {
		return fmt.Errorf("namespace %s is still terminating", name)
	}

	before := ns.DeepCopy()
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	for k, v := range labelsFor(t) {
		ns.Labels[k] = v
	}
	if equality.Semantic.DeepEqual(before.Labels, ns.Labels) {
		return nil
	}
	recordChange("Namespace", controllerutil.OperationResultUpdated)
	return r.Patch(ctx, ns, client.MergeFrom(before))
}

func (r *TenantReconciler) ensureQuota(ctx context.Context, t *tenancyv1alpha1.Tenant, ns string) error {
	rq := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: ns}}
	q := t.Spec.Quota
	if q == nil || (q.CPU == nil && q.Memory == nil && q.Pods == nil) {
		return r.deleteIfExists(ctx, "ResourceQuota", rq)
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rq, func() error {
		mergeLabels(&rq.ObjectMeta, labelsFor(t))
		hard := corev1.ResourceList{}
		if q.CPU != nil {
			hard[corev1.ResourceRequestsCPU] = *q.CPU
		}
		if q.Memory != nil {
			hard[corev1.ResourceRequestsMemory] = *q.Memory
		}
		if q.Pods != nil {
			hard[corev1.ResourcePods] = *resource.NewQuantity(int64(*q.Pods), resource.DecimalSI)
		}
		rq.Spec.Hard = hard
		return controllerutil.SetControllerReference(t, rq, r.Scheme)
	})
	recordChange("ResourceQuota", op)
	return err
}

func (r *TenantReconciler) ensureNetworkPolicy(ctx context.Context, t *tenancyv1alpha1.Tenant, ns string) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyName, Namespace: ns}}
	if t.Spec.Isolation == tenancyv1alpha1.IsolationNone {
		return r.deleteIfExists(ctx, "NetworkPolicy", np)
	}

	// Strict (the default): pods accept ingress only from pods in the same
	// namespace. Egress is left open so workloads can still reach DNS.
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		mergeLabels(&np.ObjectMeta, labelsFor(t))
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
			}},
		}
		return controllerutil.SetControllerReference(t, np, r.Scheme)
	})
	recordChange("NetworkPolicy", op)
	return err
}

func (r *TenantReconciler) ensureOwnerBinding(ctx context.Context, t *tenancyv1alpha1.Tenant, ns string) error {
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: OwnersBindingName, Namespace: ns}}
	owners := slices.Clone(t.Spec.Owners)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	if len(owners) == 0 {
		return r.deleteIfExists(ctx, "RoleBinding", rb)
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		mergeLabels(&rb.ObjectMeta, labelsFor(t))
		// RoleRef is immutable once created, so it is only ever set to this value.
		rb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "admin"}
		subjects := make([]rbacv1.Subject, 0, len(owners))
		for _, o := range owners {
			subjects = append(subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: o})
		}
		rb.Subjects = subjects
		return controllerutil.SetControllerReference(t, rb, r.Scheme)
	})
	recordChange("RoleBinding", op)
	return err
}

// finalize deletes the tenant namespace (which garbage-collects everything in
// it) and releases the finalizer only once the namespace is fully gone.
func (r *TenantReconciler) finalize(ctx context.Context, t *tenancyv1alpha1.Tenant) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(t, Finalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.setStatus(ctx, t, tenancyv1alpha1.PhaseTerminating, metav1.ConditionFalse,
		ReasonTerminating, "deleting tenant namespace"); err != nil {
		return ctrl.Result{}, err
	}

	name := NamespaceFor(t)
	ns := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: name}, ns)
	switch {
	case apierrors.IsNotFound(err):
		// Namespace is gone; fall through and release the finalizer.
	case err != nil:
		return ctrl.Result{}, err
	case !metav1.IsControlledBy(ns, t):
		// Never delete a namespace this tenant does not own.
	default:
		if ns.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, ns, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			recordChange("Namespace", "deleted")
			r.Recorder.Eventf(t, corev1.EventTypeNormal, "NamespaceDeleting", "deleting namespace %s", name)
		}
		return ctrl.Result{RequeueAfter: namespaceDeletePoll}, nil
	}

	controllerutil.RemoveFinalizer(t, Finalizer)
	return ctrl.Result{}, r.Update(ctx, t)
}

func (r *TenantReconciler) deleteIfExists(ctx context.Context, kind string, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	recordChange(kind, "deleted")
	return nil
}

func labelsFor(t *tenancyv1alpha1.Tenant) map[string]string {
	return map[string]string{ManagedByLabel: ManagedByValue, TenantLabel: t.Name}
}

func mergeLabels(m *metav1.ObjectMeta, labels map[string]string) {
	if m.Labels == nil {
		m.Labels = map[string]string{}
	}
	for k, v := range labels {
		m.Labels[k] = v
	}
}

// SetupWithManager registers the controller. Owned objects are watched so that
// drift (someone editing or deleting a quota, policy or binding) is repaired.
// Per-item exponential backoff is combined with an overall token bucket so a
// burst of failing tenants cannot hammer the API server.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	limiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](500*time.Millisecond, 5*time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(20), 200)},
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(&tenancyv1alpha1.Tenant{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Namespace{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent, RateLimiter: limiter}).
		Named("tenant").
		Complete(r)
}
