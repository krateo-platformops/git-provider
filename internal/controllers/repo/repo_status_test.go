package repo

import (
	"context"
	"errors"
	"testing"

	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFailSyncMarksResourceUnavailableAndSyncedFalse(t *testing.T) {
	cr := &repov1alpha1.Repo{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "test-system"},
	}
	cr.Status.SetConditions(commonv1.Available())

	e := &external{}

	inputErr := errors.New("push failed")
	returnedErr := e.failSync(context.Background(), cr, inputErr)

	require.ErrorIs(t, returnedErr, inputErr)

	ready := cr.GetCondition(commonv1.TypeReady)
	synced := cr.GetCondition(commonv1.TypeSynced)

	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, commonv1.ReasonUnavailable, ready.Reason)
	require.Equal(t, metav1.ConditionFalse, synced.Status)
	require.Equal(t, commonv1.ReasonReconcileError, synced.Reason)
	require.Equal(t, "push failed", synced.Message)
}
