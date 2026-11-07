package controllers

import (
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/controller"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateoplatformops/git-provider/apis"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/option"
)

// Each reconciler that takes the governor registers with it, which raises its ceiling by
// MaxConcurrentReconciles. So the ceiling shows that every controller Setup builds is governed: one
// that forgets the option is silently ungated, and nothing else would fail.
func TestSetupGovernsEveryReconciler(t *testing.T) {
	// The manager is never started, so nothing connects to this address.
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	require.NoError(t, apis.AddToScheme(mgr.GetScheme()))

	o := controller.Options{
		Logger:                  logging.NewNopLogger(),
		MaxConcurrentReconciles: 5,
		RateLimit: controller.RateLimitOptions{
			Enabled: true,
		},
	}
	require.NoError(t, o.SetupRateLimitGovernor(mgr))
	require.NotNil(t, o.RateLimitGovernor)

	require.NoError(t, Setup(mgr, option.SetupOptions{Controller: option.ControllerOptions{Options: o}}))

	assert.Equal(t, 2*o.MaxConcurrentReconciles, o.RateLimitGovernor.Ceiling())
}
