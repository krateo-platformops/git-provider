package git

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit/httpsignal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/krateoplatformops/git-provider/internal/clients/git/gittest"
)

const (
	signalBranch = "main"
	signalKey    = "git.example.com|ns/creds/token"
)

// throttles are the refusals the governor has to act on: GitHub's secondary limit and Azure
// DevOps's block (a 429 with Retry-After), and GitHub's primary limit (a 403 with no quota left).
var throttles = map[string]struct {
	status int
	header func() map[string]string
}{
	"429 with Retry-After": {
		status: http.StatusTooManyRequests,
		header: func() map[string]string { return map[string]string{"Retry-After": "120"} },
	},
	"403 with X-RateLimit-Remaining 0": {
		status: http.StatusForbidden,
		header: func() map[string]string {
			return map[string]string{
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Reset":     strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10),
			}
		},
	},
}

func newGovernor(t *testing.T) *ratelimit.Governor {
	t.Helper()
	g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1})
	require.NoError(t, err)
	return g
}

// runtimeContext is what the runtime and the controllers hand the git client: the governor as the
// sink, and the key the controller gates this remote on.
func runtimeContext(g *ratelimit.Governor) context.Context {
	return httpsignal.WithKey(ratelimit.WithSink(context.Background(), g), signalKey)
}

// assertDeferred fails unless the governor now defers calls for signalKey until a server deadline.
func assertDeferred(t *testing.T, g *ratelimit.Governor) {
	t.Helper()
	_, d := g.Acquire(context.Background(), signalKey)
	require.NotNil(t, d, "the throttled response never reached the governor: calls for %q are still let through", signalKey)
	assert.Equal(t, ratelimit.DeferThrottled, d.Reason)
	assert.True(t, d.Until.After(time.Now()), "the deferral carries no server deadline: %+v", d)
}

// Each remote operation must reach the governor from every request it makes. Each case refuses one
// step of one operation, so a call that drops the context fails its own case: go-git's context-free
// forms run on context.Background(), and the signal is then lost with no error and no log.
func TestThrottledResponseReachesTheGovernor(t *testing.T) {
	type env struct {
		ctx    context.Context
		server *gittest.Server
		opts   ListOptions
		home   string
	}
	clone := func(t *testing.T, e env) *Repo {
		t.Helper()
		// A clone the test needs to get going. Its own requests go to the same server, before the
		// refusal, on a context with no sink.
		r, err := Clone(context.Background(), CloneOptions{URL: e.opts.URL, Branch: signalBranch, HomeDir: e.home})
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Cleanup() })
		return r
	}
	commit := func(t *testing.T, r *Repo) {
		t.Helper()
		f, err := r.FS().Create("pushed.txt")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		_, err = r.Commit(".", "pushed", &IndexOptions{})
		require.NoError(t, err)
	}

	cases := map[string]struct {
		step gittest.Step
		// prepare runs before the refusal is switched on.
		prepare func(t *testing.T, e env) *Repo
		run     func(e env, r *Repo) error
	}{
		"GetLatestCommitRemote": {
			step: gittest.AdvertiseUpload,
			run: func(e env, _ *Repo) error {
				_, err := GetLatestCommitRemote(e.ctx, e.opts)
				return err
			},
		},
		"Clone, listing the branch": {
			step: gittest.AdvertiseUpload,
			run: func(e env, _ *Repo) error {
				_, err := Clone(e.ctx, CloneOptions{URL: e.opts.URL, Branch: signalBranch, HomeDir: e.home})
				return err
			},
		},
		"Clone, downloading objects": {
			step: gittest.UploadPack,
			run: func(e env, _ *Repo) error {
				_, err := Clone(e.ctx, CloneOptions{URL: e.opts.URL, Branch: signalBranch, HomeDir: e.home})
				return err
			},
		},
		"IsInGitCommitHistory": {
			step: gittest.UploadPack,
			run: func(e env, _ *Repo) error {
				_, err := IsInGitCommitHistory(e.ctx, e.opts, "0000000000000000000000000000000000000001")
				return err
			},
		},
		"IsFuncInGitCommitHistory": {
			step: gittest.UploadPack,
			run: func(e env, _ *Repo) error {
				_, err := IsFuncInGitCommitHistory(e.ctx, e.opts, func(*object.Commit) bool { return false })
				return err
			},
		},
		"Pull": {
			step:    gittest.AdvertiseUpload,
			prepare: clone,
			run:     func(e env, r *Repo) error { return Pull(e.ctx, r, false) },
		},
		"Push, listing the remote": {
			step: gittest.AdvertiseReceive,
			prepare: func(t *testing.T, e env) *Repo {
				r := clone(t, e)
				commit(t, r)
				return r
			},
			run: func(e env, r *Repo) error { return r.Push(e.ctx, "origin", signalBranch, false) },
		},
		"Push, uploading objects": {
			step: gittest.ReceivePack,
			prepare: func(t *testing.T, e env) *Repo {
				r := clone(t, e)
				commit(t, r)
				return r
			},
			run: func(e env, r *Repo) error { return r.Push(e.ctx, "origin", signalBranch, false) },
		},
		"Push to the default refspec": {
			step: gittest.AdvertiseReceive,
			prepare: func(t *testing.T, e env) *Repo {
				r := clone(t, e)
				commit(t, r)
				return r
			},
			run: func(e env, r *Repo) error { return r.Push(e.ctx, "origin", "", false) },
		},
	}

	for throttleName, th := range throttles {
		for name, tc := range cases {
			t.Run(throttleName+"/"+name, func(t *testing.T) {
				origin := gittest.NewRepo(t, signalBranch)
				origin.Commit("first commit")
				srv := gittest.Serve(t, origin)
				g := newGovernor(t)
				e := env{
					ctx:    runtimeContext(g),
					server: srv,
					opts:   ListOptions{URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir()},
					home:   t.TempDir(),
				}

				var r *Repo
				if tc.prepare != nil {
					r = tc.prepare(t, e)
				}
				srv.Refuse(th.status, th.header(), tc.step)

				require.Error(t, tc.run(e, r), "the refused request must fail the operation")
				assertDeferred(t, g)
			})
		}
	}
}

// The cookie path installs its own client for the length of the operation, so it needs the signal
// transport too, and has to put defaultClient back: go-git's own client would drop every signal
// from then on, in the whole process.
func TestThrottledResponseReachesTheGovernorWithACookie(t *testing.T) {
	origin := gittest.NewRepo(t, signalBranch)
	origin.Commit("first commit")
	srv := gittest.ServeTLS(t, origin)
	srv.Refuse(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, gittest.AdvertiseUpload)
	g := newGovernor(t)

	_, err := GetLatestCommitRemote(runtimeContext(g), ListOptions{
		URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir(), Insecure: true, GitCookies: testCookie,
	})
	require.Error(t, err)
	assertDeferred(t, g)

	ep, err := transport.NewEndpoint("https://example.com/o/r.git")
	require.NoError(t, err)
	got, err := gitclient.NewClient(ep)
	require.NoError(t, err)
	assert.Same(t, defaultClient, got)
}

// spec.insecure is an API field, and go-git refuses it for a wrapped transport: the session fails
// with "expected underlying client transport to be of type: *http.Transport". The first half is
// that regression. The second proves the option is what lets the client through: without it the
// same server is rejected on its certificate, so insecure really does skip verification and
// nothing else does.
func TestInsecureRemotesKeepWorkingAndReportSignals(t *testing.T) {
	origin := gittest.NewRepo(t, signalBranch)
	want := origin.Commit("first commit")
	srv := gittest.ServeTLS(t, origin)
	opts := ListOptions{URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir(), Insecure: true}

	got, err := GetLatestCommitRemote(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, want, *got)

	r, err := Clone(context.Background(), CloneOptions{URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir(), Insecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Cleanup() })

	f, err := r.FS().Create("pushed.txt")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = r.Commit(".", "pushed", &IndexOptions{})
	require.NoError(t, err)
	require.NoError(t, r.Push(context.Background(), "origin", signalBranch, true))

	srv.Refuse(http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, gittest.AdvertiseUpload)
	g := newGovernor(t)
	_, err = GetLatestCommitRemote(runtimeContext(g), opts)
	require.Error(t, err)
	assertDeferred(t, g)

	srv.Healthy()
	opts.Insecure = false
	_, err = GetLatestCommitRemote(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate", "without insecure the self-signed server must be rejected")
}

// The transport must not throttle a healthy server or change what go-git sees: a whole clone and
// push succeed and leave the key at its ceiling.
func TestHealthyResponsesAreNotThrottled(t *testing.T) {
	origin := gittest.NewRepo(t, signalBranch)
	origin.Commit("first commit")
	srv := gittest.Serve(t, origin)
	// GitHub sends these on every response.
	srv.Annotate(map[string]string{
		"X-RateLimit-Limit":     "5000",
		"X-RateLimit-Remaining": "4999",
		"X-RateLimit-Reset":     strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
	})
	g := newGovernor(t)
	ctx := runtimeContext(g)

	r, err := Clone(ctx, CloneOptions{URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Cleanup() })
	f, err := r.FS().Create("pushed.txt")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = r.Commit(".", "pushed", &IndexOptions{})
	require.NoError(t, err)
	require.NoError(t, r.Push(ctx, "origin", signalBranch, false))

	release, d := g.Acquire(context.Background(), signalKey)
	require.Nil(t, d)
	release()
	assert.Equal(t, g.Ceiling(), g.Capacity(signalKey))
}

// An advisory on a successful response must arrive too: Azure DevOps announces its delay on a 200,
// before it starts refusing.
func TestAdvisoryOnASuccessfulResponseReachesTheGovernor(t *testing.T) {
	origin := gittest.NewRepo(t, signalBranch)
	origin.Commit("first commit")
	srv := gittest.Serve(t, origin)
	srv.Annotate(map[string]string{"Retry-After": "5", "X-RateLimit-Delay": "1.5"})

	g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 8})
	require.NoError(t, err)

	_, err = GetLatestCommitRemote(runtimeContext(g), ListOptions{URL: srv.URL, Branch: signalBranch, HomeDir: t.TempDir()})
	require.NoError(t, err)
	assert.Less(t, g.Capacity(signalKey), g.Ceiling(), "the advisory never reached the governor")
}

// The caller's deadline has to stop an operation the server never answers. A remote call used to
// run with no deadline at all, so a hung server held its worker, and now its governor slot, for
// good. Each case hangs one step of one operation.
func TestHungRemoteStopsAtTheCallersDeadline(t *testing.T) {
	const deadline = 300 * time.Millisecond

	cases := map[string]struct {
		step    gittest.Step
		prepare bool
		run     func(ctx context.Context, url, home string, r *Repo) error
	}{
		"listing refs": {
			step: gittest.AdvertiseUpload,
			run: func(ctx context.Context, url, home string, _ *Repo) error {
				_, err := GetLatestCommitRemote(ctx, ListOptions{URL: url, Branch: signalBranch, HomeDir: home})
				return err
			},
		},
		"cloning": {
			step: gittest.UploadPack,
			run: func(ctx context.Context, url, home string, _ *Repo) error {
				_, err := Clone(ctx, CloneOptions{URL: url, Branch: signalBranch, HomeDir: home})
				return err
			},
		},
		"reading the history": {
			step: gittest.UploadPack,
			run: func(ctx context.Context, url, home string, _ *Repo) error {
				_, err := IsInGitCommitHistory(ctx, ListOptions{URL: url, Branch: signalBranch, HomeDir: home}, "0")
				return err
			},
		},
		"pushing": {
			step:    gittest.ReceivePack,
			prepare: true,
			run: func(ctx context.Context, _, _ string, r *Repo) error {
				return r.Push(ctx, "origin", signalBranch, false)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			origin := gittest.NewRepo(t, signalBranch)
			origin.Commit("first commit")
			srv := gittest.Serve(t, origin)
			home := t.TempDir()

			var r *Repo
			if tc.prepare {
				var err error
				r, err = Clone(context.Background(), CloneOptions{URL: srv.URL, Branch: signalBranch, HomeDir: home})
				require.NoError(t, err)
				t.Cleanup(func() { _ = r.Cleanup() })
				f, err := r.FS().Create("pushed.txt")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				_, err = r.Commit(".", "pushed", &IndexOptions{})
				require.NoError(t, err)
			}
			srv.Hang(tc.step)

			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- tc.run(ctx, srv.URL, home, r) }()

			select {
			case err := <-done:
				require.Error(t, err)
				assert.Contains(t, err.Error(), context.DeadlineExceeded.Error())
			case <-time.After(30 * time.Second):
				t.Fatal("the operation outlived its deadline: a hung server would hold this worker and its slot for good")
			}
		})
	}
}

// Installing the client is process-wide, so it must be exactly as wide as it needs to be: ssh, git
// and file keep go-git's own clients, and both HTTP schemes get ours. A remote on plain http is
// usual for a self-hosted git server.
func TestOnlyHTTPSchemesUseTheSignalClient(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		assert.Same(t, defaultClient, gitclient.Protocols[scheme], "scheme %s", scheme)
	}
	for _, scheme := range []string{"ssh", "git", "file"} {
		_, isSignal := gitclient.Protocols[scheme].(*signalClient)
		assert.False(t, isSignal, "scheme %s must keep go-git's own client", scheme)
		assert.NotNil(t, gitclient.Protocols[scheme], "scheme %s", scheme)
	}
}
