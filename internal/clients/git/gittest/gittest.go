// Package gittest builds throwaway git repositories on local disk for tests.
//
// The controllers' Observe reads remote history through the git client, which accepts a local
// path as a remote URL. A repository made here lets a test drive Observe down a chosen path with
// no network and no git server. go-git serves a local path by running the git-upload-pack binary,
// so git must be installed where the tests run. Server puts one on HTTP for a test that needs the
// responses of a git server, such as rate limits.
package gittest

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Repo is a non-bare repository in a test's temporary directory.
type Repo struct {
	t       testing.TB
	repo    *git.Repository
	commits int
	// URL is the repository's path, usable wherever the git client takes a remote URL.
	URL string
}

// NewRepo creates an empty repository whose default branch is branch.
func NewRepo(t testing.TB, branch string) *Repo {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	})
	if err != nil {
		t.Fatalf("initialising test repository: %v", err)
	}
	return &Repo{t: t, repo: r, URL: dir}
}

// Commit adds a commit with the given message on the current branch and returns its hash. Each
// commit changes a file, so it is never empty.
func (r *Repo) Commit(message string) string {
	r.t.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		r.t.Fatalf("opening worktree: %v", err)
	}
	r.commits++
	name := "file-" + strconv.Itoa(r.commits) + ".txt"
	if err := os.WriteFile(filepath.Join(r.URL, name), []byte(message), 0o600); err != nil {
		r.t.Fatalf("writing %s: %v", name, err)
	}
	if _, err := wt.Add(name); err != nil {
		r.t.Fatalf("staging %s: %v", name, err)
	}
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: "gittest", Email: "gittest@example.com", When: time.Now()},
	})
	if err != nil {
		r.t.Fatalf("committing: %v", err)
	}
	return hash.String()
}

// Step is one kind of request a git client makes to a smart HTTP server.
type Step string

const (
	// AdvertiseUpload is the GET of the refs before a list, clone, fetch or pull.
	AdvertiseUpload Step = "advertise-upload"
	// UploadPack is the POST that downloads objects.
	UploadPack Step = "upload-pack"
	// AdvertiseReceive is the GET of the refs before a push.
	AdvertiseReceive Step = "advertise-receive"
	// ReceivePack is the POST that uploads objects.
	ReceivePack Step = "receive-pack"
)

// AllSteps refuses every kind of request.
var AllSteps = []Step{AdvertiseUpload, UploadPack, AdvertiseReceive, ReceivePack}

// stepOf names the step r is, or "" for anything else.
func stepOf(r *http.Request) Step {
	switch {
	case strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
		return AdvertiseUpload
	case strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-receive-pack":
		return AdvertiseReceive
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		return UploadPack
	case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		return ReceivePack
	}
	return ""
}

// refusal is the response a refused step gets.
type refusal struct {
	status int
	header map[string]string
}

// Server serves one Repo over smart HTTP with the real `git http-backend`. A test refuses chosen
// steps with Refuse, the way a rate-limiting server does, or adds headers to every response with
// Annotate. So go-git runs for real against a real server, and every step can be refused on its
// own.
type Server struct {
	// URL is the repository's remote URL on this server.
	URL string

	mu       sync.Mutex
	refused  map[Step]refusal
	hung     map[Step]bool
	headers  map[string]string
	requests int
	// release frees the handlers Hang is holding, so the server can shut down even when a client
	// never gave up on them.
	release chan struct{}
}

// Serve starts a plain HTTP server for repo.
func Serve(t testing.TB, repo *Repo) *Server {
	t.Helper()
	s, h := newServer(t, repo)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(s.release) }) // cleanups run last first, so this goes before Close.
	s.URL = srv.URL + "/" + filepath.Base(repo.URL)
	return s
}

// ServeTLS starts a server for repo with a certificate no client trusts, for testing the insecure
// option.
func ServeTLS(t testing.TB, repo *Repo) *Server {
	t.Helper()
	s, h := newServer(t, repo)
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(s.release) }) // cleanups run last first, so this goes before Close.
	s.URL = srv.URL + "/" + filepath.Base(repo.URL)
	return s
}

func newServer(t testing.TB, repo *Repo) (*Server, http.Handler) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("finding git: %v", err)
	}
	// repo is checked out, and these servers exist to be pushed to.
	if out, err := exec.Command(gitPath, "-C", repo.URL, "config", "receive.denyCurrentBranch", "ignore").CombinedOutput(); err != nil {
		t.Fatalf("configuring test repository: %v: %s", err, out)
	}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		// REMOTE_USER makes http-backend accept pushes.
		Env:        []string{"GIT_PROJECT_ROOT=" + filepath.Dir(repo.URL), "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=gittest"},
		InheritEnv: []string{"PATH", "HOME"},
	}
	s := &Server{refused: map[Step]refusal{}, hung: map[Step]bool{}, headers: map[string]string{}, release: make(chan struct{})}
	return s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		ref, refused := s.refused[stepOf(r)]
		hung := s.hung[stepOf(r)]
		for k, v := range s.headers {
			w.Header().Set(k, v)
		}
		s.mu.Unlock()
		if hung {
			select {
			case <-r.Context().Done():
			case <-s.release:
			}
			return
		}
		if refused {
			for k, v := range ref.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(ref.status)
			return
		}
		backend.ServeHTTP(w, r)
	})
}

// Refuse answers the given steps with status and header until Healthy is called.
func (s *Server) Refuse(status int, header map[string]string, steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, step := range steps {
		s.refused[step] = refusal{status: status, header: header}
	}
}

// Hang accepts the given steps and never answers, the way a stuck server does.
func (s *Server) Hang(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, step := range steps {
		s.hung[step] = true
	}
}

// Annotate adds header to every response.
func (s *Server) Annotate(header map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = header
}

// Healthy clears Refuse and Annotate.
func (s *Server) Healthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = map[Step]refusal{}
	s.headers = map[string]string{}
}

// Requests is how many requests reached the server, refused or not.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Replay makes s answer the way the response at baseURL does: a refusal is applied to every step,
// and the rate-limit headers of a success are added to s's real git responses. It connects
// provider-runtime's reconcilertest helpers, which serve one recorded response to every request,
// to go-git, which needs a real server to get as far as a successful response.
func (s *Server) Replay(t testing.TB, baseURL string) {
	t.Helper()
	resp, err := http.Get(baseURL) //nolint:gosec // the test's own httptest server.
	if err != nil {
		t.Fatalf("reading the recorded response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	header := map[string]string{}
	for k := range resp.Header {
		if k == "Retry-After" || strings.HasPrefix(k, "X-Ratelimit-") {
			header[k] = resp.Header.Get(k)
		}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		s.Refuse(resp.StatusCode, header, AllSteps...)
		return
	}
	s.Annotate(header)
}
