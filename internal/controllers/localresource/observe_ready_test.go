package localresource

import (
	"context"
	"errors"
	"testing"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	localresourcev1alpha1 "github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/clients/git/gittest"
	credentialhelper "github.com/krateoplatformops/git-provider/internal/controllers/common/credentialHelper"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/footer"
)

const branch = "main"

func newObservedLocalResource(url, content string) *localresourcev1alpha1.LocalResource {
	return &localresourcev1alpha1.LocalResource{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test-system"},
		Spec: localresourcev1alpha1.LocalResourceSpec{
			FromResource: localresourcev1alpha1.FromResource{FromString: &content},
			ToRepo:       localresourcev1alpha1.LocalResourceOpts{Url: url, Branch: branch},
		},
	}
}

// commitFor commits to target as the provider does for cr, with the footer that names cr and
// the hash of its spec.
func commitFor(t *testing.T, target *gittest.Repo, cr *localresourcev1alpha1.LocalResource) string {
	t.Helper()
	f, err := footer.LocalResourceCommitFooter(context.Background(), cr, nil)
	require.NoError(t, err)
	return target.Commit("update\n\n" + f)
}

func newObserveExternal() *external {
	return &external{log: logging.NewNopLogger(), cfg: &gitClientOpts{ToRepoCreds: &credentialhelper.Credentials{}}}
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
		// setup prepares the target repository and the LocalResource.
		setup func(t *testing.T, target *gittest.Repo, cr *localresourcev1alpha1.LocalResource)
		// wantObservation pins the path the case takes; wantReady is what that path reports.
		wantObservation reconciler.ExternalObservation
		wantReady       commonv1.ConditionReason
	}{
		// The deletion shortcut. Synced=ReconcileError is what a failed Observe leaves next to
		// NotObserved, so this path does run right after one.
		"DeletedAfterReconcileError": {
			setup: func(_ *testing.T, _ *gittest.Repo, cr *localresourcev1alpha1.LocalResource) {
				cr.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
				cr.Status.SetConditions(commonv1.ReconcileError(errors.New("observe failed")))
			},
			wantObservation: absent,
			wantReady:       commonv1.ReasonDeleting,
		},
		"NotCommittedYet": {
			setup: func(_ *testing.T, target *gittest.Repo, _ *localresourcev1alpha1.LocalResource) {
				target.Commit("someone else's commit")
			},
			wantObservation: absent,
			wantReady:       commonv1.ReasonUnavailable,
		},
		// The branch holds a commit for an earlier spec of this LocalResource. Update would
		// commit the current one, but it does nothing while syncEnabled is false; Ready must
		// still be set.
		"DriftedWithSyncDisabled": {
			setup: func(t *testing.T, target *gittest.Repo, cr *localresourcev1alpha1.LocalResource) {
				earlier := cr.DeepCopy()
				older := "kind: ConfigMap # earlier"
				earlier.Spec.FromResource.FromString = &older
				cr.Status.TargetCommitId = commitFor(t, target, earlier)
				cr.Spec.SyncEnabled = false
			},
			wantObservation: drifted,
			wantReady:       commonv1.ReasonAvailable,
		},
		"UpToDate": {
			setup: func(t *testing.T, target *gittest.Repo, cr *localresourcev1alpha1.LocalResource) {
				cr.Status.TargetCommitId = commitFor(t, target, cr)
			},
			wantObservation: current,
			wantReady:       commonv1.ReasonAvailable,
		},
		// Create found the target already holding the content, so it recorded the branch head
		// without making a commit, and there is no footer to find. The recorded commit is what
		// the resource is.
		"RecordedWithoutFooter": {
			setup: func(_ *testing.T, target *gittest.Repo, cr *localresourcev1alpha1.LocalResource) {
				cr.Status.TargetCommitId = target.Commit("content already there")
			},
			wantObservation: current,
			wantReady:       commonv1.ReasonAvailable,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			target := gittest.NewRepo(t, branch)
			cr := newObservedLocalResource(target.URL, "kind: ConfigMap")
			tc.setup(t, target, cr)

			obs, err := newObserveExternal().Observe(context.Background(), cr.DeepCopy())
			require.NoError(t, err)
			require.Equal(t, tc.wantObservation, obs, "the case does not take the path it names")

			reconcilertest.AssertObserveSetsReady(t, context.Background(), newObserveExternal(), cr)
			require.Equal(t, tc.wantReady, cr.GetCondition(commonv1.TypeReady).Reason)
		})
	}
}

// An Observe that could not read the target has not assessed Ready, so its error must stay plain:
// the runtime then reports Ready=NotObserved, which is the truthful answer.
func TestObserveUnreadableTargetIsNotReadyAssessed(t *testing.T) {
	target := gittest.NewRepo(t, branch)
	cr := newObservedLocalResource(target.URL+"-does-not-exist", "kind: ConfigMap")

	_, err := newObserveExternal().Observe(context.Background(), cr)

	require.Error(t, err)
	require.False(t, reconciler.IsReadyAssessed(err), "an Observe that could not tell must not claim it set Ready: %v", err)
}
