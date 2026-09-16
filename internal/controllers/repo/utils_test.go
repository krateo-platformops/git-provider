package repo

import (
	"context"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"
	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoadExternalClientOpts(t *testing.T) {
	ctx := context.TODO()
	kc := fake.NewFakeClient()

	// Create the Repo object

	cr := &repov1alpha1.Repo{
		Spec: repov1alpha1.RepoSpec{
			FromRepo: repov1alpha1.FromRepoOpts{
				RepoOpts: repov1alpha1.RepoOpts{
					AuthMethod: "bearer",
					SecretRef: &commonv1.SecretKeySelector{
						Key: "token",
						Reference: commonv1.Reference{
							Name:      "from-repo-secret",
							Namespace: "default",
						},
					},
				},
			},
			ToRepo: repov1alpha1.RepoOpts{
				AuthMethod: "generic",
				SecretRef: &commonv1.SecretKeySelector{
					Key: "token",
					Reference: commonv1.Reference{
						Name:      "to-repo-secret",
						Namespace: "default",
					},
				},
			},
			Insecure:                true,
			UnsupportedCapabilities: false,
		},
	}

	// Create the secret objects
	fromRepoSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "from-repo-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("from-repo-token"),
		},
	}
	toRepoSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "to-repo-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("to-repo-token"),
		},
	}

	// Create the secrets in the cluster
	require.NoError(t, kc.Create(ctx, fromRepoSecret))
	require.NoError(t, kc.Create(ctx, toRepoSecret))

	opts, err := loadExternalClientOpts(ctx, kc, cr)
	require.NoError(t, err)

	expectedOpts := &externalClientOpts{
		Insecure:                true,
		UnsupportedCapabilities: false,
		FromRepoCreds: &githttp.TokenAuth{
			Token: "from-repo-token",
		},
		ToRepoCreds: &githttp.BasicAuth{
			Username: "krateoctl",
			Password: "to-repo-token",
		},
		FromRepoCookieFile: nil,
		ToRepoCookieFile:   nil,
	}

	assert.Equal(t, expectedOpts, opts)
}

func TestGetRepoCookies(t *testing.T) {
	ctx := context.TODO()
	kc := fake.NewFakeClient()

	// Create the secret object
	secretData := map[string][]byte{
		"cookie": []byte("repo-cookie"),
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "repo-secret",
			Namespace: "default",
		},
		Data: secretData,
	}

	// Create the secret in the cluster
	require.NoError(t, kc.Create(ctx, secret))

	opts := repov1alpha1.RepoOpts{
		SecretRef: &commonv1.SecretKeySelector{
			Key: "cookie",
			Reference: commonv1.Reference{
				Name:      "repo-secret",
				Namespace: "default",
			},
		},
	}

	cookies, err := getRepoCookies(ctx, kc, opts)
	require.NoError(t, err)

	expectedCookies := []byte("repo-cookie")
	assert.Equal(t, expectedCookies, cookies)
}
