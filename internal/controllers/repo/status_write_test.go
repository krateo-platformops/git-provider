package repo

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
)

// A status write that loses one optimistic-concurrency race after a successful push must still
// land, with the status the sync produced and the external name it set (#26). Before the fix the
// 409 escaped Create, provider-runtime requeued without recording an outcome, and the Repo wedged
// on external-create-pending with the push already on the remote.
func TestStatusWriteSurvivesAConflictAfterThePush(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, repov1alpha1.SchemeBuilder.AddToScheme(scheme))

	stored := &repov1alpha1.Repo{ObjectMeta: metav1.ObjectMeta{Name: "publish-x-source", Namespace: "krateo-system"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).WithStatusSubresource(stored).Build()
	e := &external{kube: kube, log: logging.NewNopLogger()}
	ctx := context.Background()

	// The reconciler's copy, as Create holds it after the push: status and external name set.
	cr := &repov1alpha1.Repo{}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(stored), cr))
	meta.SetExternalName(cr, "29dd9b15c0ab")
	cr.Status.TargetCommitId = "29dd9b15c0ab"
	cr.Status.TargetBranch = "builder/x"

	// Someone else writes the object in the meantime, so the reconciler's resourceVersion is stale:
	// a write of `cr` as it stands is a REAL 409, on every attempt, until the helper re-reads.
	concurrent := &repov1alpha1.Repo{}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(stored), concurrent))
	concurrent.SetLabels(map[string]string{"concurrent": "writer"})
	require.NoError(t, kube.Update(ctx, concurrent))
	stale := cr.DeepCopy()
	err := kube.Status().Update(ctx, stale)
	require.True(t, apierrors.IsConflict(err), "the setup must produce a real conflict, or this test proves nothing: %v", err)

	require.NoError(t, e.updateStatusWithRetry(ctx, cr))

	got := &repov1alpha1.Repo{}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(stored), got))
	require.Equal(t, "29dd9b15c0ab", got.Status.TargetCommitId)
	require.Equal(t, "builder/x", got.Status.TargetBranch)
	require.Equal(t, "writer", got.GetLabels()["concurrent"], "the concurrent write must survive — the helper re-reads, it does not overwrite")
	require.Equal(t, "29dd9b15c0ab", meta.GetExternalName(cr), "the external name must survive — the runtime persists it as the create's record")
}

// Every status write in the sync path goes through updateStatusWithRetry — the LocalResource guard
// (#17 fixed one write and missed its twin in the already-up-to-date branch), applied here.
func TestNoBareStatusUpdateOutsideTheRetryHelper(t *testing.T) {
	src, err := os.ReadFile("repo.go")
	require.NoError(t, err)
	helper := regexp.MustCompile(`(?s)func \(e \*external\) updateStatusWithRetry\(.*?\n}`)
	outside := helper.ReplaceAllString(string(src), "")
	var offenders []string
	for _, line := range strings.Split(outside, "\n") {
		if strings.Contains(line, "Status().Update(") {
			offenders = append(offenders, strings.TrimSpace(line))
		}
	}
	require.Empty(t, offenders, "bare Status().Update outside updateStatusWithRetry — a lost conflict here strands the Repo on external-create-pending")
}
