package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// These complement controller-runtime's built-in controller_runtime_* and
// workqueue_* metrics, which are served from the same /metrics endpoint.
var (
	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tenant_operator_reconcile_duration_seconds",
		Help:    "Time spent in the Tenant reconcile loop, by result.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
	}, []string{"result"})

	resourceChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tenant_operator_resource_changes_total",
		Help: "Writes made to managed resources, by kind and operation. A steady rate with no spec changes points to drift or a fight with another controller.",
	}, []string{"kind", "operation"})
)

func init() {
	metrics.Registry.MustRegister(reconcileDuration, resourceChanges)
}

func observeReconcile(start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	reconcileDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

func recordChange(kind string, op controllerutil.OperationResult) {
	if op == controllerutil.OperationResultNone || op == "" {
		return
	}
	resourceChanges.WithLabelValues(kind, string(op)).Inc()
}
