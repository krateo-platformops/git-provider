// Package ratelimitkey names the rate-limited scope of a git remote, for provider-runtime's
// rate-limit governor. It is shared by the repo and localresource controllers so the key a call is
// gated on and the key its signals are reported under can't drift apart: a signal under any other
// string throttles a key no call is gated on.
package ratelimitkey

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	commonv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit/httpsignal"
)

// For returns the scope of calls to rawURL made with the credential ref points at:
// host|namespace/name/key, or the host alone when there is no credential, which is the anonymous
// quota. Git servers count quota per identity, so two credentials on one host are two scopes. The
// host is httpsignal.HostKey of the URL. The credential is named by its reference, never its value,
// because a key that changes when the token is rotated splits one quota into several.
//
// It returns "" for a remote that can't report a signal, which the runtime reads as not gated per
// endpoint: anything but http or https (ssh, git, a local path) has no response headers to read,
// and a URL that doesn't parse never produces a request.
//
// For parses strings and does no I/O, so a RateLimitKeyer can call it.
func For(rawURL string, ref *commonv1.SecretKeySelector) string {
	if !isHTTP(rawURL) {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := httpsignal.HostKey(u)
	if host == "" {
		return ""
	}
	if ref == nil {
		return host
	}
	return fmt.Sprintf("%s|%s/%s/%s", host, ref.Namespace, ref.Name, ref.Key)
}

// WithRemote returns the context for calls to one remote: their signals are reported under
// For(rawURL, ref) rather than the bare request host.
func WithRemote(ctx context.Context, rawURL string, ref *commonv1.SecretKeySelector) context.Context {
	return httpsignal.WithKey(ctx, For(rawURL, ref))
}

// isHTTP compares the scheme by hand because an scp-like remote, git@host:org/repo.git, is not a
// URL and url.Parse refuses it.
func isHTTP(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
