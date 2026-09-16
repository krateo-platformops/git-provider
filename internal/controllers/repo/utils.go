package repo

import (
	"context"
	"fmt"
	"strings"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	repov1alpha1 "github.com/krateoplatformops/git-provider/apis/repo/v1alpha1"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type externalClientOpts struct {
	Insecure                bool
	UnsupportedCapabilities bool
	FromRepoCreds           transport.AuthMethod
	ToRepoCreds             transport.AuthMethod
	FromRepoCookieFile      []byte
	ToRepoCookieFile        []byte
}

func loadExternalClientOpts(ctx context.Context, kc client.Client, cr *repov1alpha1.Repo) (*externalClientOpts, error) {
	var fromRepoCookie, toRepoCookie []byte
	fromRepoCreds, err := getRepoCredentials(ctx, kc, cr.Spec.FromRepo.RepoOpts)
	if err != nil {
		return nil, fmt.Errorf("retrieving .fromRepo credentials: %w", err)
	}
	fromRepoCookie = nil
	if fromRepoCreds == nil {
		fromRepoCookie, err = getRepoCookies(ctx, kc, cr.Spec.FromRepo.RepoOpts)
		if err != nil {
			return nil, fmt.Errorf("retrieving .fromRepo cookies: %w", err)
		}
	}

	toRepoCreds, err := getRepoCredentials(ctx, kc, cr.Spec.ToRepo)
	if err != nil {
		return nil, fmt.Errorf("retrieving .toRepo credentials: %w", err)
	}
	if toRepoCreds == nil {
		toRepoCookie, err = getRepoCookies(ctx, kc, cr.Spec.ToRepo)
		if err != nil {
			return nil, fmt.Errorf("retrieving .toRepo cookies: %w", err)
		}
	}

	return &externalClientOpts{
		Insecure:                cr.Spec.Insecure,
		UnsupportedCapabilities: cr.Spec.UnsupportedCapabilities,
		FromRepoCreds:           fromRepoCreds,
		ToRepoCreds:             toRepoCreds,
		FromRepoCookieFile:      fromRepoCookie,
		ToRepoCookieFile:        toRepoCookie,
	}, nil
}

func getRepoCookies(ctx context.Context, k client.Client, opts repov1alpha1.RepoOpts) ([]byte, error) {
	if opts.SecretRef == nil {
		return nil, nil
	}

	sec, err := resource.GetSecret(ctx, k, opts.SecretRef)

	return []byte(sec), err
}

// getRepoCredentials returns the from repo credentials stored in a secret.
func getRepoCredentials(ctx context.Context, k client.Client, opts repov1alpha1.RepoOpts) (transport.AuthMethod, error) {
	if opts.SecretRef == nil {
		return nil, nil
	}

	token, err := resource.GetSecret(ctx, k, opts.SecretRef)
	if err != nil {
		return nil, err
	}

	if strings.EqualFold(opts.AuthMethod, "bearer") {
		return &githttp.TokenAuth{
			Token: token,
		}, nil
	}

	if strings.EqualFold(opts.AuthMethod, "cookiefile") {
		return nil, nil
	}

	username := "krateoctl"
	if opts.UsernameRef != nil {
		username, err = resource.GetSecret(ctx, k, opts.UsernameRef)
		if err != nil {
			return nil, err
		}
	}

	return &githttp.BasicAuth{
		Username: username,
		Password: token,
	}, nil
}
