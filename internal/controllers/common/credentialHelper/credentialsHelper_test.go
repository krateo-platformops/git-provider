package credentialhelper

import (
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// The rule the localresource connector used to disagree with: bearer and cookiefile do not consume
// a username, so requiring a usernameRef for them is wrong. Both the LocalResource CRD field
// description and docs/local-resource.md document them as ignoring it.
func TestUsesUsername(t *testing.T) {
	for _, tc := range []struct {
		authMethod string
		want       bool
	}{
		{"bearer", false},
		{"BEARER", false},
		{"cookiefile", false},
		{"CookieFile", false},
		{"basic", true},
		{"generic", true},
		{"", true}, // unset falls through to basic in GetCredentials
	} {
		if got := UsesUsername(tc.authMethod); got != tc.want {
			t.Errorf("UsesUsername(%q) = %v, want %v", tc.authMethod, got, tc.want)
		}
	}
}

// UsesUsername must agree with what GetCredentials actually does with Username, or a caller that
// trusts it will resolve a secret nobody reads — or skip one that is needed.
func TestUsesUsernameAgreesWithGetCredentials(t *testing.T) {
	const sentinel = "sentinel-username"

	for _, authMethod := range []string{"bearer", "cookiefile", "basic", "generic", ""} {
		creds, err := GetCredentials(CredentialHelperOpts{
			AuthMethod: authMethod,
			Username:   sentinel,
			Token:      "a-token",
		})
		if err != nil {
			t.Fatalf("GetCredentials(%q): unexpected error: %v", authMethod, err)
		}

		basic, isBasic := creds.Transport.(*githttp.BasicAuth)
		consumed := isBasic && basic.Username == sentinel

		if consumed != UsesUsername(authMethod) {
			t.Errorf("authMethod %q: GetCredentials consumed username = %v, UsesUsername = %v",
				authMethod, consumed, UsesUsername(authMethod))
		}
	}
}

func TestGetCredentialsRequiresToken(t *testing.T) {
	if _, err := GetCredentials(CredentialHelperOpts{AuthMethod: "bearer"}); err == nil {
		t.Fatal("expected an error for an empty token, got nil")
	}
}
