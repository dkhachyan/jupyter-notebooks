// Command notebook-route-controller exposes Kubeflow Notebooks through
// Gateway API HTTPRoutes with per-notebook Envoy Gateway authorization.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"notebook-route-controller/internal/access"
	"notebook-route-controller/internal/config"
	"notebook-route-controller/internal/controller"
	"notebook-route-controller/internal/nsapi"
	"notebook-route-controller/internal/routes"
)

// runOnEveryReplica ignores leader election: every replica serves the
// namespaces API from its own local cache view.
type runOnEveryReplica struct{ manager.Runnable }

func (runOnEveryReplica) NeedLeaderElection() bool { return false }

func main() {
	cfg := config.Default()
	cfg.Bind(flag.CommandLine)

	var (
		metricsAddr    string
		probeAddr      string
		leaderElect    bool
		maxConcurrency int
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to (0 disables it).")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for high availability.")
	flag.IntVar(&maxConcurrency, "max-concurrent-reconciles", 4, "Maximum number of notebooks reconciled concurrently.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "invalid configuration")
		os.Exit(1)
	}

	peers, err := cfg.Peers()
	if err != nil {
		setupLog.Error(err, "invalid configuration")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "building scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "notebook-route-controller.kubeflow.org",
	})
	if err != nil {
		setupLog.Error(err, "creating manager")
		os.Exit(1)
	}

	reconciler := &controller.NotebookReconciler{
		Client: mgr.GetClient(),
		Resolver: &access.Resolver{
			Reader:                 mgr.GetClient(),
			Reviewer:               access.SubjectAccessReviewer{Client: mgr.GetClient()},
			Verb:                   cfg.AccessVerb,
			UserPrefix:             cfg.UserPrefix,
			GroupPrefix:            cfg.GroupPrefix,
			GroupsEnabled:          cfg.GroupsClaim != "",
			IgnoredSubjectPrefixes: cfg.IgnoredSubjectPrefixes,
		},
		Recorder: mgr.GetEventRecorder(routes.ManagerName),
		Config:   cfg,
		Peers:    peers,
	}
	if err := reconciler.SetupWithManager(mgr, maxConcurrency); err != nil {
		setupLog.Error(err, "setting up controller")
		os.Exit(1)
	}

	if bindAddr := cfg.NamespacesAPIBindAddress; bindAddr != "" {
		srv := &http.Server{
			Addr:              bindAddr,
			Handler:           &nsapi.Handler{Reader: mgr.GetClient(), Reviewer: reconciler.Resolver.Reviewer, UserHeader: cfg.NamespacesAPIUserHeader, TTL: cfg.NamespacesAPICacheTTL},
			ReadHeaderTimeout: 10 * time.Second,
		}
		err := mgr.Add(runOnEveryReplica{manager.RunnableFunc(func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})})
		if err != nil {
			setupLog.Error(err, "adding the namespaces API server")
			os.Exit(1)
		}
		setupLog.Info("serving the namespaces API", "address", bindAddr)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding ready check")
		os.Exit(1)
	}

	setupLog.Info("starting", "gateway", cfg.GatewayNamespace+"/"+cfg.GatewayName, "verb", cfg.AccessVerb)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "running manager")
		os.Exit(1)
	}
}
