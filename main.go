package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/go-logr/logr"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/logger"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	"github.com/krateoplatformops/git-provider/apis"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateoplatformops/git-provider/internal/controllers"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"

	"github.com/stoewer/go-strcase"
)

const (
	providerName = "Git"
)

func main() {
	envVarPrefix := fmt.Sprintf("%s_PROVIDER", strcase.UpperSnakeCase(providerName))

	debug := flag.Bool("debug", env.Bool(fmt.Sprintf("%s_DEBUG", envVarPrefix), false), "Run with debug logging.")
	syncPeriod := flag.Duration("sync", env.Duration(fmt.Sprintf("%s_SYNC_PERIOD", envVarPrefix), time.Hour), "Controller manager sync period such as 300ms, 1.5h, or 2h45m")
	pollInterval := flag.Duration("poll", env.Duration(fmt.Sprintf("%s_POLL_INTERVAL", envVarPrefix), 3*time.Minute), "Poll interval controls how often an individual resource should be checked for drift.")
	maxReconcileRate := flag.Int("max-reconcile-rate", env.Int(fmt.Sprintf("%s_MAX_RECONCILE_RATE", envVarPrefix), 5), "The number of concurrent reconciles for each controller. This is the maximum number of resources that can be reconciled at the same time.")
	leaderElection := flag.Bool("leader-election", env.Bool(fmt.Sprintf("%s_LEADER_ELECTION", envVarPrefix), false), "Use leader election for the controller manager.")
	maxErrorRetryInterval := flag.Duration("max-error-retry-interval", env.Duration(fmt.Sprintf("%s_MAX_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Minute), "The maximum interval between retries when an error occurs. This should be less than the half of the poll interval.")
	minErrorRetryInterval := flag.Duration("min-error-retry-interval", env.Duration(fmt.Sprintf("%s_MIN_ERROR_RETRY_INTERVAL", envVarPrefix), 1*time.Second), "The minimum interval between retries when an error occurs. This should be less than max-error-retry-interval.")
	timeout := flag.Duration("timeout", env.Duration(fmt.Sprintf("%s_TIMEOUT", envVarPrefix), 4*time.Minute), "The timeout for each reconcile.")

	gitCommitAuthorName := flag.String("git-commit-author-name", env.String(fmt.Sprintf("%s_GIT_COMMIT_AUTHOR_NAME", envVarPrefix), "krateo-git-provider"), "The name to use for git commits.")
	gitCommitAuthorEmail := flag.String("git-commit-author-email", env.String(fmt.Sprintf("%s_GIT_COMMIT_AUTHOR_EMAIL", envVarPrefix), "contact@krateo.io"), "The email to use for git commits.")

	flag.Parse()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "/tmp" // this folder is guaranteed to be always writable
	}
	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}

	// The fork's plumbing has no slogs/pretty. Use the shared structured handler instead, which is
	// what every other Krateo component emits: one JSON object per line in the OTel log model, so
	// git-provider's logs become logs-ingester compatible like the rest of the platform. This is a
	// deliberate behaviour change from the previous coloured console output.
	lh := logger.NewHandler(logLevel == slog.LevelDebug, os.Stderr)

	logrlog := logr.FromSlogHandler(slog.New(lh).Handler())
	log := logging.NewLogrLogger(logrlog)

	log.WithValues("sync-period", syncPeriod.String()).
		WithValues("poll-interval", pollInterval.String()).
		WithValues("max-reconcile-rate", *maxReconcileRate).
		WithValues("leader-election", *leaderElection).
		WithValues("min-error-retry-interval", minErrorRetryInterval.String()).
		WithValues("max-error-retry-interval", maxErrorRetryInterval.String()).
		WithValues("git-commit-author-name", *gitCommitAuthorName).
		WithValues("git-commit-author-email", *gitCommitAuthorEmail).
		WithValues("timeout", timeout.String()).
		Info("Starting Git Provider")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "Cannot get API server rest config, trying in-cluster config")
		os.Exit(1)
	}

	ctrl.SetLogger(logrlog)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		LeaderElection:   *leaderElection,
		LeaderElectionID: fmt.Sprintf("leader-election-%s-provider", strcase.KebabCase(providerName)),
		Cache: cache.Options{
			SyncPeriod: syncPeriod,
		},
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
	})
	if err != nil {
		log.Error(err, "Cannot create controller manager")
		os.Exit(1)
	}

	o := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobalExponential(*minErrorRetryInterval, *maxErrorRetryInterval),
	}

	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Error(err, "Cannot add APIs to scheme")
		os.Exit(1)
	}
	if err := controllers.Setup(mgr, option.SetupOptions{
		Controller: option.ControllerOptions{
			Options: o,
			Timeout: *timeout,
		},
		Git: option.GitOptions{
			CommitAuthorName:  *gitCommitAuthorName,
			CommitAuthorEmail: *gitCommitAuthorEmail,
			HomeDir:           homeDir,
		},
	}); err != nil {
		log.Error(err, "Cannot setup controllers")
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Cannot start controller manager")
		os.Exit(1)
	}
}
