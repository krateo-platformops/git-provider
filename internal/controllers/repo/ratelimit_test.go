package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	plumbingevent "github.com/krateo-platformops/plumbing/kubeutil/event"
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
	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git/gittest"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/ratelimitkey"
)

func secretRef(name string) *commonv1.SecretKeySelector {
	return &commonv1.SecretKeySelector{
		Reference: commonv1.Reference{Name: name, Namespace: "team-a"},
		Key:       "token",
	}
}

func repoBetween(from, to repov1alpha1.RepoOpts) *repov1alpha1.Repo {
	return &repov1alpha1.Repo{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "team-a"},
		Spec:       repov1alpha1.RepoSpec{FromRepo: repov1alpha1.FromRepoOpts{RepoOpts: from}, ToRepo: to},
	}
}

func remote(url, secret string) repov1alpha1.RepoOpts {
	return repov1alpha1.RepoOpts{Url: url, Branch: branch, SecretRef: secretRef(secret)}
}

func TestRateLimitKeys(t *testing.T) {
	const github = "https://github.com/org/repo"
	cases := map[string]struct {
		from, to repov1alpha1.RepoOpts
		want     []string
	}{
		"one host and one credential is one scope": {
			remote(github+"-a", "gh-token"), remote(github+"-b", "gh-token"),
			[]string{"github.com|team-a/gh-token/token"},
		},
		"the auth method and username do not change the scope": {
			repov1alpha1.RepoOpts{Url: github, SecretRef: secretRef("gh-token"), AuthMethod: "bearer"},
			repov1alpha1.RepoOpts{Url: github, SecretRef: secretRef("gh-token"), UsernameRef: secretRef("gh-user")},
			[]string{"github.com|team-a/gh-token/token"},
		},
		"two credentials on one host are two scopes": {
			remote(github, "read-token"), remote(github, "write-token"),
			[]string{"github.com|team-a/read-token/token", "github.com|team-a/write-token/token"},
		},
		"two hosts are two scopes": {
			remote("https://source.developers.google.com/p/r", "gcp-cookie"), remote(github, "git-provider"),
			[]string{"source.developers.google.com|team-a/gcp-cookie/token", "github.com|team-a/git-provider/token"},
		},
		"anonymous on both ends of one host": {
			repov1alpha1.RepoOpts{Url: github}, repov1alpha1.RepoOpts{Url: github}, []string{"github.com"},
		},
		"ssh has no signals to gate on": {
			remote("git@github.com:org/repo.git", "gh-token"), remote("git@github.com:org/repo.git", "gh-token"), []string{""},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A client with no clients of its own: reading a Secret panics on the nil client.
			got, err := (&external{}).RateLimitKeys(repoBetween(tc.from, tc.to))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Each key must be the exact string that remote's signals carry, or a refusal throttles a key no
// call is gated on.
func TestRateLimitKeysAreTheKeysSignalsReport(t *testing.T) {
	cr := repoBetween(remote("https://a.example.com/r", "read-token"), remote("https://b.example.com/r", "write-token"))
	got, err := (&external{}).RateLimitKeys(cr)
	require.NoError(t, err)
	assert.Equal(t, []string{keyOf(cr, true), keyOf(cr, false)}, got)
}

// The runtime calls RateLimitKeys before every gated call, also while the endpoint is throttled.
func TestRateLimitKeysDoesNoIO(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()

	// The external has no clients, so a read of the Secret panics on the nil client.
	got, err := (&external{}).RateLimitKeys(repoBetween(remote(srv.URL, "gh-token"), remote(srv.URL, "other-token")))
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Zero(t, requests, "RateLimitKeys reached the git server")
}

func TestRateLimitKeysRejectsAnotherKind(t *testing.T) {
	_, err := (&external{}).RateLimitKeys(&resourcefake.Managed{})
	require.Error(t, err)
}

func governedReconciler(t *testing.T, cr *repov1alpha1.Repo, g *ratelimit.Governor, extra ...reconciler.ReconcilerOption) (*reconciler.Reconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, apis.AddToScheme(scheme))
	objects := []client.Object{cr}
	for _, name := range []string{"gh-token", "read-token", "write-token"} {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Data:       map[string][]byte{"token": []byte("a-token")},
		})
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(cr).Build()

	r := reconciler.NewReconciler(&resourcefake.Manager{Client: kube, Scheme: scheme},
		resource.ManagedKind(repov1alpha1.RepoGroupVersionKind),
		append([]reconciler.ReconcilerOption{
			reconciler.WithExternalConnecter(&connector{
				kube:     kube,
				log:      logging.NewNopLogger(),
				recorder: events.NewFakeRecorder(1000),
			}),
			reconciler.WithTimeout(30 * time.Second),
			reconciler.WithRateLimitGovernor(g),
		}, extra...)...,
	)
	return r, kube
}

// The chain the governor depends on, end to end, for each kind of call a Repo makes to its remotes:
// a real server refuses a real go-git operation run by the real connector and reconciler, and the
// next pass is deferred without touching the server. Each case fails on its own if that operation
// drops the context, because the signal is then lost with no error.
func TestThrottledRemoteDefersTheNextPass(t *testing.T) {
	throttles := map[string]struct {
		status int
		header map[string]string
	}{
		"429 with Retry-After": {http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}},
		"403 with X-RateLimit-Remaining 0": {http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10),
		}},
	}
	calls := map[string]struct {
		step gittest.Step
		// synced is a Repo that already pushed, so its pass observes the remotes; otherwise the
		// pass creates, which syncs them.
		synced bool
	}{
		"Observe listing fromRepo":   {step: gittest.AdvertiseUpload, synced: true},
		"Observe cloning toRepo":     {step: gittest.UploadPack, synced: true},
		"Create cloning the remotes": {step: gittest.UploadPack},
		"Create pushing to toRepo":   {step: gittest.AdvertiseReceive},
		"Create uploading the push":  {step: gittest.ReceivePack},
	}
	for throttleName, th := range throttles {
		for callName, call := range calls {
			t.Run(throttleName+"/"+callName, func(t *testing.T) {
				git := gittest.NewRepo(t, branch)
				head := git.Commit("first commit")
				srv := gittest.Serve(t, git)
				srv.Refuse(th.status, th.header, call.step)

				// One host and one credential, so the Repo is keyed. The toRepo branch has
				// nothing of fromRepo's, so a sync has something to push.
				cr := repoBetween(remote(srv.URL, "gh-token"), remote(srv.URL, "gh-token"))
				cr.Spec.FromRepo.Path = "/"
				cr.Spec.ToRepo.Path = "/copied"
				if call.synced {
					cr.Status.TargetCommitId = head
					cr.Status.OriginCommitId = head
					cr.Status.TargetBranch = branch
					cr.Status.OriginBranch = branch
				}

				g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
				require.NoError(t, err)
				r, kube := governedReconciler(t, cr, g)
				req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}}

				// The pass that gets the refusal is still an error.
				_, err = r.Reconcile(context.Background(), req)
				require.Error(t, err)
				reached := srv.Requests()

				res, err := r.Reconcile(context.Background(), req)
				require.NoError(t, err, "a deferral is not a failure")
				assert.Greater(t, res.RequeueAfter, time.Minute, "the requeue must honour the server's deadline")
				assert.LessOrEqual(t, res.RequeueAfter, 2*time.Minute+time.Second)
				assert.Equal(t, reached, srv.Requests(), "a deferred pass reached the git server")

				got := &repov1alpha1.Repo{}
				require.NoError(t, kube.Get(context.Background(), req.NamespacedName, got))
				synced := got.GetCondition(commonv1.TypeSynced)
				assert.Equal(t, commonv1.ReasonReconcileDeferred, synced.Reason)
				assert.Contains(t, synced.Message, "|team-a/gh-token/token is rate-limited by the server")
			})
		}
	}
}

// A Repo between two remotes is the case one key could not describe. Whichever remote refuses, the
// next pass is deferred before it touches either of them, and the object names the remote that
// refused. Each case runs the real reconciler and go-git against two real servers, so it fails if
// the Repo is gated on only one of its remotes.
func TestMixedRepoIsDeferredByEitherRemote(t *testing.T) {
	calls := map[string]struct {
		fromRefuses bool
		step        gittest.Step
		synced      bool
	}{
		"Observe, fromRepo refuses the list": {fromRefuses: true, step: gittest.AdvertiseUpload, synced: true},
		"Observe, toRepo refuses the clone":  {step: gittest.AdvertiseUpload, synced: true},
		"Create, toRepo refuses the clone":   {step: gittest.AdvertiseUpload},
		"Create, fromRepo refuses the clone": {fromRefuses: true, step: gittest.UploadPack},
		"Create, toRepo refuses the push":    {step: gittest.ReceivePack},
	}
	for name, tc := range calls {
		t.Run(name, func(t *testing.T) {
			f := newObserveFixture(t)
			f.origin.Commit("a file the target does not have yet, so a sync has something to push")
			fromSrv, toSrv := gittest.Serve(t, f.origin), gittest.Serve(t, f.target)

			cr := f.syncedRepo()
			cr.Spec.FromRepo.Url, cr.Spec.FromRepo.SecretRef = fromSrv.URL, secretRef("read-token")
			cr.Spec.ToRepo.Url, cr.Spec.ToRepo.SecretRef = toSrv.URL, secretRef("write-token")
			if !tc.synced {
				cr.Status = repov1alpha1.RepoStatus{}
			}

			refused := toSrv
			if tc.fromRefuses {
				refused = fromSrv
			}
			refused.Refuse(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, tc.step)
			refusedKey, healthyKey := keyOf(cr, tc.fromRefuses), keyOf(cr, !tc.fromRefuses)
			require.NotEqual(t, refusedKey, healthyKey)

			g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
			require.NoError(t, err)
			r, kube := governedReconciler(t, cr, g)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}}

			// The pass that gets the refusal is still an error.
			_, err = r.Reconcile(context.Background(), req)
			require.Error(t, err)
			fromRequests, toRequests := fromSrv.Requests(), toSrv.Requests()

			res, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err, "a deferral is not a failure")
			assert.Greater(t, res.RequeueAfter, time.Minute, "the requeue must honour the server's deadline")
			assert.Equal(t, fromRequests, fromSrv.Requests(), "a deferred pass reached fromRepo")
			assert.Equal(t, toRequests, toSrv.Requests(), "a deferred pass reached toRepo")

			got := &repov1alpha1.Repo{}
			require.NoError(t, kube.Get(context.Background(), req.NamespacedName, got))
			synced := got.GetCondition(commonv1.TypeSynced)
			assert.Equal(t, commonv1.ReasonReconcileDeferred, synced.Reason)
			assert.Contains(t, synced.Message, refusedKey+" is rate-limited by the server")
			assert.NotContains(t, synced.Message, healthyKey)

			// The healthy remote stays open to whatever else is gated on it.
			release, d := g.Acquire(context.Background(), healthyKey)
			require.Nil(t, d, "the refusal throttled the healthy remote")
			release()
		})
	}
}

// A server that never answers must not keep a governor slot past the reconcile timeout. The
// governor here allows one call, so the Acquire below succeeds only if the pass gave its slot back.
func TestHungRemoteGivesItsSlotBackAtTheTimeout(t *testing.T) {
	git := gittest.NewRepo(t, branch)
	head := git.Commit("first commit")
	srv := gittest.Serve(t, git)
	srv.Hang(gittest.UploadPack)

	cr := repoBetween(remote(srv.URL, "gh-token"), remote(srv.URL, "gh-token"))
	cr.Status.TargetCommitId = head
	cr.Status.OriginCommitId = head
	keys, err := (&external{}).RateLimitKeys(cr)
	require.NoError(t, err)
	require.Len(t, keys, 1)

	g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
	require.NoError(t, err)
	r, _ := governedReconciler(t, cr, g, reconciler.WithTimeout(300*time.Millisecond))
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}}

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(), req)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the pass outlived the reconcile timeout, so it still holds its governor slot")
	}

	release, d := g.AcquireAll(context.Background(), keys)
	require.Nil(t, d, "the timed-out pass kept its slot")
	release()
}

// Whatever the object is gated on, a refusal has to land on the remote that refused: a Repo's two
// remotes are often two quotas, and other objects are gated on each of them.
func TestSignalsAreReportedPerRemote(t *testing.T) {
	for name, fromRefuses := range map[string]bool{"fromRepo refuses": true, "toRepo refuses": false} {
		t.Run(name, func(t *testing.T) {
			f := newObserveFixture(t)
			fromSrv, toSrv := gittest.Serve(t, f.origin), gittest.Serve(t, f.target)
			cr := f.syncedRepo()
			cr.Spec.FromRepo.Url, cr.Spec.FromRepo.SecretRef = fromSrv.URL, secretRef("read-token")
			cr.Spec.ToRepo.Url, cr.Spec.ToRepo.SecretRef = toSrv.URL, secretRef("write-token")

			refused := toSrv
			if fromRefuses {
				refused = fromSrv
			}
			refused.Refuse(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, gittest.AllSteps...)
			refusedKey, healthyKey := keyOf(cr, fromRefuses), keyOf(cr, !fromRefuses)

			g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
			require.NoError(t, err)
			_, err = newObserveExternal().Observe(ratelimit.WithSink(context.Background(), g), cr)
			require.Error(t, err)

			_, d := g.Acquire(context.Background(), refusedKey)
			require.NotNil(t, d, "the refusal never reached the key of the remote that refused")
			assert.Equal(t, ratelimit.DeferThrottled, d.Reason)

			release, d := g.Acquire(context.Background(), healthyKey)
			require.Nil(t, d, "the healthy remote was throttled by the other one's refusal")
			release()
		})
	}
}

// The same for a sync, which makes the most calls of any pass and touches each remote more than
// once. Each remote has its own server here, so a refusal reported under the other remote's key
// fails, which a Repo with one scope could never show.
func TestSyncReportsSignalsPerRemote(t *testing.T) {
	cases := map[string]struct {
		fromRefuses bool
		step        gittest.Step
	}{
		"cloning toRepo":        {false, gittest.AdvertiseUpload},
		"cloning fromRepo":      {true, gittest.AdvertiseUpload},
		"listing toRepo's refs": {false, gittest.AdvertiseReceive},
		"pushing to toRepo":     {false, gittest.ReceivePack},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newObserveFixture(t)
			f.origin.Commit("a file the target does not have yet, so the sync has something to push")
			fromSrv, toSrv := gittest.Serve(t, f.origin), gittest.Serve(t, f.target)
			cr := f.syncedRepo()
			cr.Spec.FromRepo.Url, cr.Spec.FromRepo.SecretRef = fromSrv.URL, secretRef("read-token")
			cr.Spec.ToRepo.Url, cr.Spec.ToRepo.SecretRef = toSrv.URL, secretRef("write-token")

			refused := toSrv
			if tc.fromRefuses {
				refused = fromSrv
			}
			refused.Refuse(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, tc.step)

			g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
			require.NoError(t, err)
			e := newObserveExternal()
			e.rec = *plumbingevent.NewAPIRecorder(events.NewFakeRecorder(100))

			err = e.SyncRepos(ratelimit.WithSink(context.Background(), g), cr, "first commit")
			require.Error(t, err)

			_, d := g.Acquire(context.Background(), keyOf(cr, tc.fromRefuses))
			require.NotNil(t, d, "the refusal never reached the key of the remote that refused")
			assert.Equal(t, ratelimit.DeferThrottled, d.Reason)

			release, d := g.Acquire(context.Background(), keyOf(cr, !tc.fromRefuses))
			require.Nil(t, d, "the healthy remote was throttled by the other one's refusal")
			release()
		})
	}
}

func keyOf(cr *repov1alpha1.Repo, from bool) string {
	if from {
		return ratelimitkey.For(cr.Spec.FromRepo.Url, cr.Spec.FromRepo.SecretRef)
	}
	return ratelimitkey.For(cr.Spec.ToRepo.Url, cr.Spec.ToRepo.SecretRef)
}

// reconcilertest's conformance check, through Observe. It is the one provider-runtime asks every
// provider to run: a provider that never reports looks exactly like an endpoint never limited.
func TestReportsRateLimits(t *testing.T) {
	reconcilertest.AssertReportsRateLimits(t, func(ctx context.Context, baseURL string) error {
		f := newObserveFixture(t)
		fromSrv, toSrv := gittest.Serve(t, f.origin), gittest.Serve(t, f.target)
		fromSrv.Replay(t, baseURL)
		toSrv.Replay(t, baseURL)

		cr := f.syncedRepo()
		cr.Spec.FromRepo.Url, cr.Spec.ToRepo.Url = fromSrv.URL, toSrv.URL
		_, err := newObserveExternal().Observe(ctx, cr)
		return err
	})
}
