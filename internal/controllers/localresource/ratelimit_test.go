package localresource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	resourcefake "github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateoplatformops/git-provider/apis"
	localresourcev1alpha1 "github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git/gittest"
	credentialhelper "github.com/krateoplatformops/git-provider/internal/controllers/common/credentialHelper"
)

const governorBranch = "main"

// credentials is an anonymous git client: the fake server takes any request.
var credentials = credentialhelper.Credentials{}

func resetIn(d time.Duration) string {
	return strconv.FormatInt(time.Now().Add(d).Unix(), 10)
}

func secretRef(key string) *commonv1.SecretKeySelector {
	return &commonv1.SecretKeySelector{
		Reference: commonv1.Reference{Name: "git-creds", Namespace: "team-a"},
		Key:       key,
	}
}

func localResourceFor(url string, creds localresourcev1alpha1.Credentials) *localresourcev1alpha1.LocalResource {
	return &localresourcev1alpha1.LocalResource{
		ObjectMeta: metav1.ObjectMeta{Name: "publish-000", Namespace: "team-a"},
		Spec: localresourcev1alpha1.LocalResourceSpec{
			ToRepo: localresourcev1alpha1.LocalResourceOpts{Url: url, Branch: governorBranch, Credentials: creds},
		},
	}
}

func TestRateLimitKey(t *testing.T) {
	const url = "https://github.com/org/repo.git"
	const key = "github.com|team-a/git-creds/token"
	cases := map[string]struct {
		url   string
		creds localresourcev1alpha1.Credentials
		want  string
	}{
		"basic: the username does not change the identity's key": {url, localresourcev1alpha1.Credentials{
			AuthMethod: "basic", SecretRef: secretRef("token"), UsernameRef: secretRef("username"),
		}, key},
		"bearer":        {url, localresourcev1alpha1.Credentials{AuthMethod: "bearer", SecretRef: secretRef("token")}, key},
		"cookiefile":    {url, localresourcev1alpha1.Credentials{AuthMethod: "cookiefile", SecretRef: secretRef("token")}, key},
		"no credential": {url, localresourcev1alpha1.Credentials{}, "github.com"},
		"ssh remote":    {"git@github.com:org/repo.git", localresourcev1alpha1.Credentials{SecretRef: secretRef("token")}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A client with no clients of its own: reading the Secret, or anything else in the
			// cluster, panics on the nil client.
			got, err := (&external{}).RateLimitKey(localResourceFor(tc.url, tc.creds))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The runtime calls RateLimitKey before every gated call, also while the endpoint is throttled. So
// it can't touch the cluster or the git server, and the value of the Secret can't be in the key.
func TestRateLimitKeyDoesNoIO(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()

	cr := localResourceFor(srv.URL, localresourcev1alpha1.Credentials{AuthMethod: "bearer", SecretRef: secretRef("token")})

	// The external has no clients, so a read of the Secret panics on the nil client.
	got, err := (&external{}).RateLimitKey(cr)
	require.NoError(t, err)
	assert.NotEmpty(t, got)
	assert.NotContains(t, got, "a-token")
	assert.Zero(t, requests, "RateLimitKey reached the git server")
}

func TestRateLimitKeyRejectsAnotherKind(t *testing.T) {
	_, err := (&external{}).RateLimitKey(&resourcefake.Managed{})
	require.Error(t, err)
}

func governedReconciler(t *testing.T, cr *localresourcev1alpha1.LocalResource, g *ratelimit.Governor) (*reconciler.Reconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apis.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "team-a"},
		Data:       map[string][]byte{"token": []byte("a-token")},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, secret).WithStatusSubresource(cr).Build()

	r := reconciler.NewReconciler(&resourcefake.Manager{Client: kube, Scheme: scheme},
		resource.ManagedKind(localresourcev1alpha1.LocalResourceGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			kube:     kube,
			log:      logging.NewNopLogger(),
			recorder: events.NewFakeRecorder(100),
			homeDir:  t.TempDir(),
		}),
		reconciler.WithTimeout(30*time.Second),
		reconciler.WithRateLimitGovernor(g),
	)
	return r, kube
}

// The chain the governor depends on, end to end: a real server refuses a real go-git operation run
// by the real connector and reconciler, and the next pass is deferred without touching the server.
// It fails if the context stops reaching the request, if the transport isn't installed, or if the
// key the calls are gated on is not the key the signal is reported under.
func TestThrottledRemoteDefersTheNextPass(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		header map[string]string
	}{
		"429 with Retry-After": {http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}},
		"403 with X-RateLimit-Remaining 0": {http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     resetIn(2 * time.Minute),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := gittest.NewRepo(t, governorBranch)
			repo.Commit("first commit")
			srv := gittest.Serve(t, repo)
			srv.Refuse(tc.status, tc.header, gittest.AllSteps...)

			cr := localResourceFor(srv.URL, localresourcev1alpha1.Credentials{AuthMethod: "bearer", SecretRef: secretRef("token")})
			g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
			require.NoError(t, err)
			r, kube := governedReconciler(t, cr, g)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}}

			// The pass that gets the refusal is still an error.
			_, err = r.Reconcile(context.Background(), req)
			require.Error(t, err)
			reached := srv.Requests()
			require.NotZero(t, reached)

			res, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err, "a deferral is not a failure")
			assert.Greater(t, res.RequeueAfter, time.Minute, "the requeue must honour the server's deadline")
			assert.LessOrEqual(t, res.RequeueAfter, 2*time.Minute+time.Second)
			assert.Equal(t, reached, srv.Requests(), "a deferred pass reached the git server")

			got := &localresourcev1alpha1.LocalResource{}
			require.NoError(t, kube.Get(context.Background(), req.NamespacedName, got))
			synced := got.GetCondition(commonv1.TypeSynced)
			assert.Equal(t, commonv1.ReasonReconcileDeferred, synced.Reason)
			assert.Contains(t, synced.Message, "|team-a/git-creds/token is rate-limited by the server")
		})
	}
}

// reconcilertest's conformance check, through Observe. It is the one provider-runtime asks every
// provider to run: a provider that never reports looks exactly like an endpoint never limited.
func TestReportsRateLimits(t *testing.T) {
	reconcilertest.AssertReportsRateLimits(t, func(ctx context.Context, baseURL string) error {
		repo := gittest.NewRepo(t, governorBranch)
		repo.Commit("first commit")
		srv := gittest.Serve(t, repo)
		srv.Replay(t, baseURL)

		e := &external{log: logging.NewNopLogger(), cfg: &gitClientOpts{
			ToRepoCreds: &credentials,
			HomeDir:     t.TempDir(),
		}}
		_, err := e.Observe(ctx, localResourceFor(srv.URL, localresourcev1alpha1.Credentials{SecretRef: secretRef("token")}))
		return err
	})
}
