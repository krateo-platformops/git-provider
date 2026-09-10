package repo

import (
	"context"
	"testing"

	commonv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"
	"github.com/krateoplatformops/provider-runtime/pkg/logging"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newExternalWithConfigMap(t *testing.T, data map[string]string) *external {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl-values", Namespace: "test-system"},
		Data:       data,
	}
	return &external{
		kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build(),
		log:  logging.NewNopLogger(),
	}
}

func ref(key string) *commonv1.ConfigMapKeySelector {
	return &commonv1.ConfigMapKeySelector{
		Reference: commonv1.Reference{Name: "tpl-values", Namespace: "test-system"},
		Key:       key,
	}
}

// The quiet trigger. provider-runtime's GetConfigMapValue returns `string(cm.Data[ref.Key])`,
// which for an ABSENT key is "" with a NIL error — so a typo in configMapKeyRef.key does not
// surface as a missing-key error, it surfaces as unparseable JSON. loadValuesFromConfigMap must
// still report that as an error rather than handing back a usable-looking nil map.
func TestLoadValuesFromConfigMapErrorsOnMissingKey(t *testing.T) {
	e := newExternalWithConfigMap(t, map[string]string{"values": `{"name":"sock-shop"}`})

	values, err := e.loadValuesFromConfigMap(context.Background(), ref("valeus")) // typo

	require.Error(t, err, "a missing key must be an error, not an empty value set")
	require.Nil(t, values)
}

// Same requirement for the other realistic operator mistake: YAML written into a key the
// controller parses as JSON.
func TestLoadValuesFromConfigMapErrorsOnNonJSON(t *testing.T) {
	e := newExternalWithConfigMap(t, map[string]string{"values": "name: sock-shop\n"})

	values, err := e.loadValuesFromConfigMap(context.Background(), ref("values"))

	require.Error(t, err)
	require.Nil(t, values)
}

func TestLoadValuesFromConfigMapReturnsValues(t *testing.T) {
	e := newExternalWithConfigMap(t, map[string]string{"values": `{"name":"sock-shop","replicas":2}`})

	values, err := e.loadValuesFromConfigMap(context.Background(), ref("values"))

	require.NoError(t, err)
	require.Equal(t, "sock-shop", values["name"])
}

// Guards the actual regression: on a load failure `values` is nil, and the templating guard
// further down (`if values != nil`) would install NO engine — so had SyncRepos continued, it
// would have copied, committed and pushed files with their `{{ }}` placeholders intact and then
// reported Available/Synced. This asserts the precondition that made that outcome possible, so a
// future refactor that drops the early return has something to fail against.
func TestLoadFailureYieldsNilValuesWhichWouldSkipTemplating(t *testing.T) {
	e := newExternalWithConfigMap(t, map[string]string{"values": `{"name":"sock-shop"}`})

	values, err := e.loadValuesFromConfigMap(context.Background(), ref("nope"))

	require.Error(t, err)
	require.Nil(t, values, "nil values is exactly what makes `if values != nil` skip the engine")
}
