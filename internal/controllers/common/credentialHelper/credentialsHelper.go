package credentialhelper

import (
	"fmt"
	"strings"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

type CredentialHelperOpts struct {
	AuthMethod string
	Username   string
	Token      string
}

type Credentials struct {
	Transport transport.AuthMethod
	Cookie    []byte
}

// DefaultUsername is used for basic auth when no usernameRef is given. Git servers that
// authenticate on a token in the password position ignore the username entirely, but it must be
// non-empty. Matches the value the repo controller already uses.
const DefaultUsername = "krateoctl"

// UsesUsername reports whether an auth method actually consumes Username. It is the single place
// that answers the question, so a caller cannot disagree with GetCredentials about it — which is
// exactly what went wrong: the localresource connector required a usernameRef for every method,
// including the two that discard it.
func UsesUsername(authMethod string) bool {
	return !strings.EqualFold(authMethod, "bearer") && !strings.EqualFold(authMethod, "cookiefile")
}

func GetCredentials(opts CredentialHelperOpts) (*Credentials, error) {
	if opts.Token == "" {
		return nil, fmt.Errorf("token is required for authentication")
	}

	if strings.EqualFold(opts.AuthMethod, "bearer") {
		return &Credentials{
			Transport: &githttp.TokenAuth{
				Token: opts.Token,
			},
			Cookie: nil,
		}, nil
	}

	if strings.EqualFold(opts.AuthMethod, "cookiefile") {
		return &Credentials{
			Transport: nil,
			Cookie:    []byte(opts.Token),
		}, nil
	}

	return &Credentials{
		Transport: &githttp.BasicAuth{
			Username: opts.Username,
			Password: opts.Token,
		},
		Cookie: nil,
	}, nil
}
