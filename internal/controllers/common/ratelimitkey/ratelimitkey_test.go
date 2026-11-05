package ratelimitkey

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit/httpsignal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var creds = &commonv1.SecretKeySelector{
	Reference: commonv1.Reference{Name: "git-creds", Namespace: "team-a"},
	Key:       "token",
}

func TestFor(t *testing.T) {
	cases := map[string]struct {
		url  string
		ref  *commonv1.SecretKeySelector
		want string
	}{
		"https remote with a credential":       {"https://github.com/org/repo.git", creds, "github.com|team-a/git-creds/token"},
		"http remote":                          {"http://gitea.local/org/repo", creds, "gitea.local|team-a/git-creds/token"},
		"a port is part of the host":           {"https://gitea.local:3000/org/repo", creds, "gitea.local:3000|team-a/git-creds/token"},
		"the host is lower-cased":              {"https://GitHub.com/org/repo", creds, "github.com|team-a/git-creds/token"},
		"the scheme's case is ignored":         {"HTTPS://github.com/org/repo", creds, "github.com|team-a/git-creds/token"},
		"no credential is the anonymous quota": {"https://github.com/org/repo", nil, "github.com"},
		"a password in the URL stays out of the key": {
			"https://someone:s3cret@github.com/org/repo", creds, "github.com|team-a/git-creds/token",
		},
		"another key of the same secret is another token": {
			"https://github.com/org/repo",
			&commonv1.SecretKeySelector{Reference: creds.Reference, Key: "other"},
			"github.com|team-a/git-creds/other",
		},
		"ssh has no response headers":      {"ssh://git@github.com/org/repo.git", creds, ""},
		"scp-like has no response headers": {"git@github.com:org/repo.git", creds, ""},
		"a local path has no server":       {"/var/repos/repo.git", creds, ""},
		"file has no server":               {"file:///var/repos/repo.git", creds, ""},
		"no URL":                           {"", creds, ""},
		"a URL with no host":               {"https:///org/repo", creds, ""},
		"a URL that does not parse":        {"https://bad host/org", creds, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, For(tc.url, tc.ref))
		})
	}
}

// The host part must be the same string httpsignal keys on: docs/usage.md requires a keyer to
// return HostKey of the base URL.
func TestForAgreesWithHostKey(t *testing.T) {
	u, err := url.Parse("https://Gitea.Local:3000/org/repo")
	require.NoError(t, err)
	assert.Equal(t, httpsignal.HostKey(u), For(u.String(), nil))
}

type recorder struct{ signals []ratelimit.Signal }

func (r *recorder) Observe(s ratelimit.Signal) { r.signals = append(r.signals, s) }

// A signal from the context WithRemote returns carries For's key. Without it, it would carry the
// bare host, and no call would be gated on that.
func TestWithRemoteReportsUnderTheKeyCallsAreGatedOn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rec := &recorder{}
	ctx := WithRemote(ratelimit.WithSink(context.Background(), rec), srv.URL, creds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: httpsignal.NewTransport(nil)}).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, rec.signals, 1)
	assert.Equal(t, For(srv.URL, creds), rec.signals[0].Key)
}
