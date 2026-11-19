// Command manager runs the tenant-operator controller manager.
package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tenancyv1alpha1 "github.com/roberto-t-plascencia/tenant-operator/api/v1alpha1"
	"github.com/roberto-t-plascencia/tenant-operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tenancyv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		leaderElect   bool
		maxConcurrent int
		kubeAPIQPS    float64
		kubeAPIBurst  int
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the Prometheus metrics endpoint binds to (0 disables it).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health and readiness probes bind to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so only one replica reconciles at a time.")
	flag.IntVar(&maxConcurrent, "max-concurrent-reconciles", 4, "Maximum number of Tenants reconciled in parallel.")
	flag.Float64Var(&kubeAPIQPS, "kube-api-qps", 20, "Client-side QPS limit for requests to the Kubernetes API server.")
	flag.IntVar(&kubeAPIBurst, "kube-api-burst", 40, "Client-side burst limit for requests to the Kubernetes API server.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	cfg.QPS = float32(kubeAPIQPS)
	cfg.Burst = kubeAPIBurst

	// Only cache objects this operator manages; unrelated namespaces, quotas,
	// policies and bindings in the cluster never enter memory.
	managed := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{controller.ManagedByLabel: controller.ManagedByValue})}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "tenant-operator.tenancy.plascencia.dev",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Namespace{}:           managed,
			&corev1.ResourceQuota{}:       managed,
			&networkingv1.NetworkPolicy{}: managed,
			&rbacv1.RoleBinding{}:         managed,
		}},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.TenantReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("tenant-operator"),
		MaxConcurrentReconciles: maxConcurrent,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
