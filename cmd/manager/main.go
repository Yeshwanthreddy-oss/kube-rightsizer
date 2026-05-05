// Command manager runs the kube-rightsizer controller: it watches
// ResourcePolicy objects, pulls historical usage from Prometheus, computes
// p95-based recommendations, and opens GitOps pull requests for changes
// that exceed each policy's configured threshold.
package main

import (
	"flag"
	"fmt"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
	"github.com/kushagrasikka/kube-rightsizer/internal/controller"
	"github.com/kushagrasikka/kube-rightsizer/internal/gitops"
	"github.com/kushagrasikka/kube-rightsizer/internal/metrics"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(rightsizerv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		prometheusAddr       string
		githubTokenEnv       string
		gitAuthorName        string
		gitAuthorEmail       string
		enableLeaderElection bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&prometheusAddr, "prometheus-address", "http://prometheus.monitoring.svc:9090", "Address of the Prometheus (or compatible) server to read usage history from.")
	flag.StringVar(&githubTokenEnv, "github-token-env", "GITHUB_TOKEN", "Name of the environment variable holding the GitHub token used to open GitOps pull requests.")
	flag.StringVar(&gitAuthorName, "git-author-name", "kube-rightsizer", "Author name used for GitOps commits.")
	flag.StringVar(&gitAuthorEmail, "git-author-email", "kube-rightsizer@users.noreply.github.com", "Author email used for GitOps commits.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	usageSource, err := metrics.NewPrometheusSource(prometheusAddr)
	if err != nil {
		setupLog.Error(err, "unable to build prometheus client")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kube-rightsizer-leader.rightsizer.slate.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	githubToken := os.Getenv(githubTokenEnv)

	reconciler := &controller.ResourcePolicyReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		UsageSource: usageSource,
		NewPRManager: func(policy *rightsizerv1alpha1.ResourcePolicy, target *rightsizerv1alpha1.GitOpsTarget) (controller.PRManager, error) {
			owner, name, err := gitops.SplitOwnerRepo(target.Repo)
			if err != nil {
				return nil, err
			}
			if githubToken == "" {
				return nil, fmt.Errorf("%s environment variable is not set; cannot open GitOps pull requests", githubTokenEnv)
			}
			cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
			return &gitops.Manager{
				Repo: &gitops.Repository{
					CloneURL:    cloneURL,
					Auth:        gitops.NewGitHubTokenAuth(githubToken),
					AuthorName:  gitAuthorName,
					AuthorEmail: gitAuthorEmail,
				},
				Opener: gitops.NewGitHubPullRequestOpener(githubToken),
				Owner:  owner,
				Name:   name,
			}, nil
		},
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourcePolicy")
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
