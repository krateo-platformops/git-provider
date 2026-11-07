package git

import (
	"crypto/tls"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit/httpsignal"
)

// defaultClient is the go-git client for every http and https remote. It is the only way the
// rate-limit governor hears from a git server: go-git does not expose response headers, so the
// signal has to be read in the HTTP transport underneath it.
var defaultClient = newSignalClient(nil)

// insecureTLS is the base transport for remotes with spec.insecure set. It is shared rather than
// built per session, so those remotes reuse connections like every other remote.
var insecureTLS = newInsecureTLS()

// init installs defaultClient before main runs, so no goroutine can be inside a go-git operation
// yet and transportMu is not needed. Only the two HTTP schemes change: ssh, git and file keep
// go-git's own clients, and they have no HTTP response to read a signal from.
func init() {
	gitclient.InstallProtocol("http", defaultClient)
	gitclient.InstallProtocol("https", defaultClient)
}

func newInsecureTLS() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.InsecureSkipVerify = true // #nosec G402 -- only for a remote whose spec.insecure asks for it.
	return t
}

// signalClient is a go-git transport whose HTTP requests go through httpsignal.Transport. A signal
// reaches the governor only when the request carries the context the runtime handed to Connect or
// the ExternalClient method, so every remote operation here must use go-git's Context form.
//
// It exists because githttp.NewClient alone can't do this. When an endpoint sets InsecureSkipTLS,
// go-git asserts that the client's Transport is an *http.Transport so it can clone and reconfigure
// it, and fails the session for any wrapper. So signalClient holds one client per TLS mode and
// clears the option from the endpoint, and go-git never needs to reconfigure anything.
//
// git-provider sets no other per-endpoint transport option (CABundle, ClientCert, ProxyOptions).
// Setting one means adding a mode here: go-git fails the session with the same assertion.
type signalClient struct {
	verified transport.Transport
	insecure transport.Transport
}

// newSignalClient returns a client using jar, which may be nil.
func newSignalClient(jar http.CookieJar) *signalClient {
	return &signalClient{
		verified: githttp.NewClient(&http.Client{Jar: jar, Transport: httpsignal.NewTransport(nil)}),
		insecure: githttp.NewClient(&http.Client{Jar: jar, Transport: httpsignal.NewTransport(insecureTLS)}),
	}
}

// pick returns the client for ep's TLS mode and the endpoint to give it. The endpoint is a copy
// when it changes, because the caller's belongs to go-git.
func (c *signalClient) pick(ep *transport.Endpoint) (transport.Transport, *transport.Endpoint) {
	if !ep.InsecureSkipTLS {
		return c.verified, ep
	}
	cleared := *ep
	cleared.InsecureSkipTLS = false
	return c.insecure, &cleared
}

func (c *signalClient) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	client, ep := c.pick(ep)
	return client.NewUploadPackSession(ep, auth)
}

func (c *signalClient) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	client, ep := c.pick(ep)
	return client.NewReceivePackSession(ep, auth)
}
