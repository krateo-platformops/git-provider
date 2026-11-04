package repo

import (
	"context"
	"errors"
	"strings"
	"testing"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git/gittest"
)

const branch = "main"

// unreachableCommit is a well-formed commit id that no fixture repository contains.
var unreachableCommit = strings.Repeat("0", 39) + "1"

// observeFixture is an origin and a target repository on local disk, each with one commit, and
// a Repo that last synced them: OriginCommitId and TargetCommitId are those commits.
type observeFixture struct {
	origin, target *gittest.Repo
	originCommit   string
	targetCommit   string
}

func newObserveFixture(t *testing.T) observeFixture {
	t.Helper()
	f := observeFixture{origin: gittest.NewRepo(t, branch), target: gittest.NewRepo(t, branch)}
	f.originCommit = f.origin.Commit("origin content")
	f.targetCommit = f.target.Commit("first commit")
	return f
}

func (f observeFixture) syncedRepo() *repov1alpha1.Repo {
	cr := &repov1alpha1.Repo{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test-system"},
		Spec: repov1alpha1.RepoSpec{
			FromRepo: repov1alpha1.FromRepoOpts{RepoOpts: repov1alpha1.RepoOpts{Url: f.origin.URL, Branch: branch}},
			ToRepo:   repov1alpha1.RepoOpts{Url: f.target.URL, Branch: branch},
		},
	}
	cr.Status.OriginCommitId = f.originCommit
	cr.Status.TargetCommitId = f.targetCommit
	cr.Status.OriginBranch = branch
	cr.Status.TargetBranch = branch
	return cr
}

func newObserveExternal() *external {
	return &external{log: logging.NewNopLogger(), cfg: &externalClientOpts{}}
}

// Every success path of Observe must set Ready (the provider-runtime v1.8.0 Ready contract). One
// case per path, so a new path without a case stands out in review, and a path that stops setting
// Ready fails here instead of as a ReadyNotSetAfterObserve Warning on a cluster.
func TestObserveSetsReady(t *testing.T) {
	var (
		absent  = reconciler.ExternalObservation{ResourceExists: false, ResourceUpToDate: true}
		drifted = reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false}
		current = reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}
	)
	cases := map[string]struct {
		mutate func(f observeFixture, cr *repov1alpha1.Repo)
		// wantObservation pins the path the case takes; wantReady is what that path reports.
		wantObservation reconciler.ExternalObservation
		wantReady       commonv1.ConditionReason
	}{
		// The deletion shortcut. Synced=ReconcileError is what a failed Observe leaves next to
		// NotObserved, so this path does run right after one.
		"DeletedAfterReconcileError": {
			mutate: func(_ observeFixture, cr *repov1alpha1.Repo) {
				cr.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
				cr.Status.SetConditions(commonv1.ReconcileError(errors.New("observe failed")))
			},
			wantObservation: absent,
			wantReady:       commonv1.ReasonDeleting,
		},
		"NotSyncedYet": {
			mutate: func(_ observeFixture, cr *repov1alpha1.Repo) {
				cr.Status = repov1alpha1.RepoStatus{}
			},
			wantObservation: absent,
			wantReady:       commonv1.ReasonUnavailable,
		},
		// The target history no longer holds the pushed commit, and enableUpdate lets Update
		// push again.
		"TargetCommitUnreachableWithUpdates": {
			mutate: func(_ observeFixture, cr *repov1alpha1.Repo) {
				cr.Spec.EnableUpdate = true
				cr.Status.TargetCommitId = unreachableCommit
			},
			wantObservation: drifted,
			wantReady:       commonv1.ReasonUnavailable,
		},
		"OriginAheadWithUpdates": {
			mutate: func(f observeFixture, cr *repov1alpha1.Repo) {
				cr.Spec.EnableUpdate = true
				f.origin.Commit("newer origin content")
			},
			wantObservation: drifted,
			wantReady:       commonv1.ReasonAvailable,
		},
		"OriginAheadWithoutUpdates": {
			mutate: func(f observeFixture, _ *repov1alpha1.Repo) {
				f.origin.Commit("newer origin content")
			},
			wantObservation: current,
			wantReady:       commonv1.ReasonAvailable,
		},
		"UpToDate": {
			mutate:          func(observeFixture, *repov1alpha1.Repo) {},
			wantObservation: current,
			wantReady:       commonv1.ReasonAvailable,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newObserveFixture(t)
			cr := f.syncedRepo()
			tc.mutate(f, cr)

			obs, err := newObserveExternal().Observe(context.Background(), cr.DeepCopy())
			require.NoError(t, err)
			require.Equal(t, tc.wantObservation, obs, "the case does not take the path it names")

			reconcilertest.AssertObserveSetsReady(t, context.Background(), newObserveExternal(), cr)
			require.Equal(t, tc.wantReady, cr.GetCondition(commonv1.TypeReady).Reason)
		})
	}
}

// With enableUpdate=false, an unreachable target commit is an error for a person to resolve
// (#23). Observe has read the target by then and set Ready=Unavailable through failSync, so the
// error is ReadyAssessed and the runtime must keep Unavailable rather than write NotObserved.
func TestObserveUnreachableTargetCommitIsReadyAssessed(t *testing.T) {
	f := newObserveFixture(t)
	cr := f.syncedRepo()
	cr.Status.TargetCommitId = unreachableCommit
	cr.Status.SetConditions(commonv1.NotObserved())

	_, err := newObserveExternal().Observe(context.Background(), cr)

	require.Error(t, err)
	require.True(t, reconciler.IsReadyAssessed(err), "Observe set Ready before failing, so the error must be ReadyAssessed: %v", err)
	require.Equal(t, unreachableTargetCommitError(cr.Status.TargetCommitId, branch, f.target.URL).Error(), err.Error(),
		"ReadyAssessed must not change the message")

	ready := cr.GetCondition(commonv1.TypeReady)
	require.Equal(t, commonv1.ReasonUnavailable, ready.Reason)
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, commonv1.ReasonReconcileError, cr.GetCondition(commonv1.TypeSynced).Reason)
}

// An Observe that could not read the remote has not assessed Ready, so its error must stay plain:
// the runtime then reports Ready=NotObserved, which is the truthful answer.
func TestObserveUnreadableRemoteIsNotReadyAssessed(t *testing.T) {
	f := newObserveFixture(t)
	cr := f.syncedRepo()
	cr.Spec.FromRepo.Url = f.origin.URL + "-does-not-exist"

	_, err := newObserveExternal().Observe(context.Background(), cr)

	require.Error(t, err)
	require.False(t, reconciler.IsReadyAssessed(err), "an Observe that could not tell must not claim it set Ready: %v", err)
}
