package git

import (
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// domain \t httponly \t path \t secure \t expires \t name \t value
var testCookie = []byte("example.com\tTRUE\t/\tTRUE\t0\tsession\tdeadbeef")

func TestCookieJarClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie []byte
		want   bool // want a custom client
	}{
		{"nil cookie", nil, false},
		{"empty cookie", []byte(""), false},
		{"newline only", []byte("\n"), false},
		{"too few fields", []byte("example.com\tTRUE\t/"), false},
		{"valid cookie", testCookie, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cookieJarClient(tc.cookie)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (got != nil) != tc.want {
				t.Fatalf("custom client = %v, want %v", got != nil, tc.want)
			}
		})
	}
}

// TestTransportRegistryIsRaceFree is the regression test for the
// external-create-pending wedge.
//
// go-git's plumbing/transport/client.Protocols is a process-global map with no
// locking: InstallProtocol writes it and getTransport reads it on every remote
// operation. Doing both at once is a fatal "concurrent map read and map write"
// that kills the process outright, stranding any managed resource whose
// krateo.io/external-create-pending annotation had already been written.
//
// Run this with -race. It must stay green: if anyone reintroduces an
// unsynchronised InstallProtocol call, this test takes the process down.
func TestTransportRegistryIsRaceFree(t *testing.T) {
	ep, err := transport.NewEndpoint("https://example.com/o/r.git")
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 2000
	var wg sync.WaitGroup

	// Readers: operations with no cookie, standing in for what go-git does
	// inside every remote operation while we hold the lock.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				release, err := lockTransport(nil, false)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := gitclient.NewClient(ep); err != nil {
					t.Error(err)
				}
				release()
			}
		}()
	}

	// Writers: the cookie-jar path, which really does mutate the global map.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				release, err := lockTransport(testCookie, false)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := gitclient.NewClient(ep); err != nil {
					t.Error(err)
				}
				release()
			}
		}()
	}

	// Capability mutators: the Azure DevOps path, which mutates a second
	// process-global (transport.UnsupportedCapabilities).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				release, err := lockTransport(nil, true)
				if err != nil {
					t.Error(err)
					return
				}
				old := transport.UnsupportedCapabilities
				transport.UnsupportedCapabilities = nil
				restoreUnsupportedCapabilities(old)
				release()
			}
		}()
	}

	wg.Wait()

	// Every writer must leave go-git's default https client installed, so that
	// the readers above are entitled to skip installing anything at all.
	got, err := gitclient.NewClient(ep)
	if err != nil {
		t.Fatal(err)
	}
	if want := githttp.DefaultClient; got != want {
		t.Fatalf("https transport not restored: got %#v, want the go-git default", got)
	}
}
