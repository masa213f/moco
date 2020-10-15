package cmd

import (
	"math/rand"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	k8smetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/cybozu-go/moco/accessor"
	mocov1alpha1 "github.com/cybozu-go/moco/api/v1alpha1"
	"github.com/cybozu-go/moco/controllers"
	"github.com/cybozu-go/moco/metrics"
	"github.com/prometheus/client_golang/prometheus"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	connMaxLifetimeFlag   = "conn-max-lifetime"
	connectionTimeoutFlag = "connection-timeout"
	readTimeoutFlag       = "read-timeout"
	waitTimeFlag          = "wait-time"
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = mocov1alpha1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func subMain() error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	rand.Seed(time.Now().UnixNano())

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		MetricsBindAddress:      config.metricsAddr,
		Port:                    9443,
		LeaderElection:          true,
		LeaderElectionID:        config.leaderElectionID,
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return err
	}

	if err = (&controllers.MySQLClusterReconciler{
		Client:                 mgr.GetClient(),
		Log:                    ctrl.Log.WithName("controllers").WithName("MySQLCluster"),
		Scheme:                 mgr.GetScheme(),
		ConfInitContainerImage: config.confInitContainerImage,
		CurlContainerImage:     config.curlContainerImage,
		MySQLAccessor: accessor.NewMySQLAccessor(&accessor.MySQLAccessorConfig{
			ConnMaxLifeTime:   config.connMaxLifeTime,
			ConnectionTimeout: config.connectionTimeout,
			ReadTimeout:       config.readTimeout,
		}),
		WaitTime: config.waitTime,
	}).SetupWithManager(mgr, 30*time.Second); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MySQLCluster")
		return err
	}
	// +kubebuilder:scaffold:builder

	metrics.RegisterMetrics(k8smetrics.Registry.(*prometheus.Registry))

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		return err
	}
	return nil
}
