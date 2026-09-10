package localresource

import (
	"context"
	"strings"
	"testing"

	commonv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"
	"github.com/krateoplatformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	localresourcev1alpha1 "github.com/krateoplatformops/git-provider/apis/localresource/v1alpha1"
)

// Regression: a bearer LocalResource with no usernameRef must connect.
//
// Connect() used to resolve UsernameRef before it read AuthMethod, so a nil ref — which the CRD
// field description and docs/local-resource.md both document as valid for bearer and cookiefile —
// failed with "retrieving .toRepo username: no credentials secret referenced". On a live cluster
// that stopped every publish: 12 of 12 LocalResources wedged at connect, none of them ever
// reaching the git server.
func TestConnectDoesNotRequireUsernameRef(t *testing.T) {
	for _, authMethod := range []string{"bearer", "cookiefile"} {
		t.Run(authMethod, func(t *testing.T) {
			c, cr := newConnectorFixture(t, authMethod, nil)

			if _, err := c.Connect(context.Background(), cr); err != nil {
				t.Fatalf("Connect with authMethod %q and no usernameRef: %v", authMethod, err)
			}
		})
	}
}

// basic still needs a username, and an explicit ref is still honoured — the fix must not turn the
// username into dead configuration for the one method that uses it.
func TestConnectResolvesUsernameForBasic(t *testing.T) {
	ref := &commonv1.SecretKeySelector{
		Reference: commonv1.Reference{Name: "git-creds", Namespace: "ns"},
		Key:       "username",
	}

	if _, err := func() (any, error) {
		c, cr := newConnectorFixture(t, "basic", ref)
		return c.Connect(context.Background(), cr)
	}(); err != nil {
		t.Fatalf("Connect with authMethod basic and a valid usernameRef: %v", err)
	}

	// A ref that points at a secret which does not exist must still be an error: silently
	// substituting the default would hide a real misconfiguration.
	missing := &commonv1.SecretKeySelector{
		Reference: commonv1.Reference{Name: "does-not-exist", Namespace: "ns"},
		Key:       "username",
	}
	c, cr := newConnectorFixture(t, "basic", missing)
	_, err := c.Connect(context.Background(), cr)
	if err == nil || !strings.Contains(err.Error(), "retrieving .toRepo username") {
		t.Fatalf("expected a .toRepo username error for a dangling ref, got %v", err)
	}
}

func newConnectorFixture(t *testing.T, authMethod string, usernameRef *commonv1.SecretKeySelector) (*connector, *localresourcev1alpha1.LocalResource) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-creds", Namespace: "ns"},
		Data: map[string][]byte{
			"token":    []byte("a-token"),
			"username": []byte("someone"),
		},
	}

	cr := &localresourcev1alpha1.LocalResource{
		ObjectMeta: metav1.ObjectMeta{Name: "publish-000", Namespace: "ns"},
		Spec: localresourcev1alpha1.LocalResourceSpec{
			ToRepo: localresourcev1alpha1.LocalResourceOpts{
				Url:    "https://github.com/org/repo.git",
				Branch: "builder/x",
				Credentials: localresourcev1alpha1.Credentials{
					AuthMethod: authMethod,
					SecretRef: &commonv1.SecretKeySelector{
						Reference: commonv1.Reference{Name: "git-creds", Namespace: "ns"},
						Key:       "token",
					},
					UsernameRef: usernameRef,
				},
			},
		},
	}

	return &connector{
		kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		log:  logging.NewNopLogger(),
	}, cr
}
