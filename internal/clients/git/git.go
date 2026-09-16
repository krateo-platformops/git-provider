package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	contexttools "github.com/krateoplatformops/provider-runtime/pkg/context"
	"github.com/krateoplatformops/provider-runtime/pkg/logging"

	"github.com/go-git/go-git/v5/plumbing/cache"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/krateoplatformops/git-provider/internal/utils"
	"github.com/krateoplatformops/plumbing/ptr"
)

var (
	CommitAuthorEmail = "contact@krateo.io"
	CommitAuthorName  = "krateo-git-provider"
)

var (
	ErrRepositoryNotFound     = fmt.Errorf("repository not found: %w", transport.ErrRepositoryNotFound)
	ErrEmptyRemoteRepository  = fmt.Errorf("remote repository is empty: %w", transport.ErrEmptyRemoteRepository)
	ErrAuthenticationRequired = fmt.Errorf("authentication required: %w", transport.ErrAuthenticationRequired)
	ErrAuthorizationFailed    = fmt.Errorf("authorization failed: %w", transport.ErrAuthorizationFailed)
	ErrBranchNotFound         = errors.New("branch not found")
	NoErrAlreadyUpToDate      = git.NoErrAlreadyUpToDate
)

type normalizedError struct {
	err error
	msg string
}

func (e normalizedError) Error() string {
	return e.msg
}

func (e normalizedError) Unwrap() error {
	return e.err
}

func normalizeEmptyReasonError(err error) error {
	if err == nil {
		return nil
	}

	msg := err.Error()
	if strings.HasSuffix(msg, ": ") {
		return normalizedError{err: err, msg: strings.TrimSuffix(msg, ": ")}
	}

	return err
}

// transportMu guards go-git's PROCESS-GLOBAL transport registry
// (plumbing/transport/client.Protocols).
//
// That registry is a plain Go map with no synchronisation of its own:
// client.InstallProtocol writes it, and client.getTransport reads it on every
// single remote operation. Mutating it while any other goroutine is inside a
// go-git remote operation is a fatal "concurrent map read and map write",
// which tears the process down immediately - no panic to recover, no deferred
// cleanup, no error returned to the reconciler.
//
// When that happens in the window between provider-runtime writing the
// krateo.io/external-create-pending annotation and writing the create result,
// the managed resource is wedged forever on "cannot determine creation result".
//
// Operations that need no custom client take the read lock and share go-git's
// default https client; only the cookie-jar and capability paths write, under
// the exclusive lock, and they restore the defaults before releasing it.
var transportMu sync.RWMutex

type Repo struct {
	rawURL      string
	auth        transport.AuthMethod
	storer      storage.Storer
	fs          billy.Filesystem
	repo        *git.Repository
	isNewBranch *bool
	cookie      []byte
	tmpDir      string
}

type CloneOptions struct {
	URL                     string
	Auth                    transport.AuthMethod
	Insecure                bool
	UnsupportedCapabilities bool
	Branch                  string
	AlternativeBranch       *string
	GitCookies              []byte
	HomeDir                 string // The home directory to use for temporary files
}

type ListOptions struct {
	URL        string
	Auth       transport.AuthMethod
	Insecure   bool
	Branch     string
	GitCookies []byte
	HomeDir    string // The home directory to use for temporary files
}

type IndexOptions struct {
	OriginRepo *Repo
	FromPath   string
	ToPath     string
}

// cookieJarClient builds the HTTP client carrying the given git cookie.
//
// It returns (nil, nil) when there is no usable cookie, which means the caller
// must leave the global transport registry completely alone - go-git's default
// https client is already installed and is exactly what we want.
func cookieJarClient(cookie []byte) (*http.Client, error) {
	cookie = bytes.Trim(cookie, "\n")
	if len(cookie) == 0 {
		return nil, nil
	}

	split := bytes.Split(cookie, []byte("\t"))
	if len(split) < 7 {
		return nil, nil
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("error creating cookie jar: %w", err)
	}

	c := &http.Cookie{
		Name:       string(split[5]),
		Value:      string(split[6]),
		RawExpires: string(split[4]),
		Path:       string(split[2]),
		Domain:     string(split[0]),
		Secure:     string(split[3]) == "TRUE",
		HttpOnly:   string(split[1]) == "TRUE",
	}

	jar.SetCookies(
		&url.URL{
			Scheme: "https",
			Host:   c.Domain,
		},
		[]*http.Cookie{
			c,
		},
	)

	return &http.Client{Jar: jar}, nil
}

// azureDevOps reports whether the URL points at Azure DevOps, which needs the
// go-git capability workaround applied by the callers below.
func azureDevOps(rawURL string) bool {
	return strings.Contains(rawURL, "dev.azure.com")
}

// lockTransport guards go-git's process-global state for the duration of a
// single remote git operation and returns the matching release function, which
// the caller must invoke exactly once - normally via defer.
//
// Set mutatesGlobals when the operation will assign to
// transport.UnsupportedCapabilities, which is a process-global too.
//
// lockTransport must NOT be called again while the returned release function is
// still outstanding: sync.RWMutex is not reentrant, so a nested call deadlocks.
// That is why the remote helpers below come in a locking exported form and a
// non-locking unexported form (see GetLatestCommitRemote/getLatestCommitRemote),
// and why the purely local operations - Branch, Commit, UpdateIndex,
// GetLatestCommit - no longer touch the registry at all.
func lockTransport(cookie []byte, mutatesGlobals bool) (func(), error) {
	custom, err := cookieJarClient(cookie)
	if err != nil {
		return nil, err
	}

	if custom == nil && !mutatesGlobals {
		// Nothing to install: go-git's default https client is already in the
		// registry. Hold the read lock so that no writer can mutate the map
		// out from under go-git while it reads it.
		transportMu.RLock()
		return transportMu.RUnlock, nil
	}

	transportMu.Lock()
	if custom == nil {
		return transportMu.Unlock, nil
	}

	gitclient.InstallProtocol("https", githttp.NewClient(custom))
	return func() {
		// Restore the exact singleton go-git ships in its registry, rather than
		// allocating an equivalent-but-different client on every release.
		gitclient.InstallProtocol("https", githttp.DefaultClient)
		transportMu.Unlock()
	}, nil
}

// GetLatestCommitRemote lists the remote refs and returns the tip of the
// requested branch. It takes the transport lock; use getLatestCommitRemote when
// the caller already holds it.
func GetLatestCommitRemote(opts ListOptions) (*string, error) {
	release, err := lockTransport(opts.GitCookies, false)
	if err != nil {
		return nil, err
	}
	defer release()

	return getLatestCommitRemote(opts)
}

// getLatestCommitRemote assumes the caller already holds the transport lock.
func getLatestCommitRemote(opts ListOptions) (*string, error) {
	tmpDir, err := os.MkdirTemp(opts.HomeDir, "git-provider-list-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	diskFS := osfs.New(tmpDir)
	dotGitFS, err := diskFS.Chroot(".git")
	if err != nil {
		return nil, fmt.Errorf("failed to create .git directory: %w", err)
	}

	storer := filesystem.NewStorage(dotGitFS, cache.NewObjectLRUDefault())
	res := &Repo{
		rawURL: opts.URL,
		auth:   opts.Auth,
		storer: storer,
		fs:     diskFS,
		cookie: opts.GitCookies,
		tmpDir: tmpDir,
	}

	res.repo, err = git.Init(res.storer, res.fs)
	if err != nil {
		return nil, err
	}
	remote, err := res.repo.CreateRemote(&config.RemoteConfig{
		URLs: []string{opts.URL},
		Name: "origin",
	})
	if err != nil {
		return nil, err
	}

	refs, err := remote.List(&git.ListOptions{
		Auth:            opts.Auth,
		InsecureSkipTLS: opts.Insecure,
	})
	if err != nil {
		return nil, normalizeEmptyReasonError(err)
	}
	repoRef := plumbing.NewBranchReferenceName(opts.Branch)
	for _, ref := range refs {
		if ref.Name() == repoRef {
			return ptr.To(ref.Hash().String()), nil
		}
	}

	return nil, fmt.Errorf("%w: branch %s reference %s not found on remote %s", ErrBranchNotFound, opts.Branch, repoRef, opts.URL)
}

func restoreUnsupportedCapabilities(oldUnsupportedCaps []capability.Capability) {
	transport.UnsupportedCapabilities = oldUnsupportedCaps
}

func isInGitCommitHistory(ctx context.Context, opts ListOptions, hash string) (bool, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	tmpDir, err := os.MkdirTemp(opts.HomeDir, "git-provider-history-*")
	if err != nil {
		return false, fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	diskFS := osfs.New(tmpDir)
	dotGitFS, err := diskFS.Chroot(".git")
	if err != nil {
		return false, fmt.Errorf("failed to create .git directory: %w", err)
	}

	storer := filesystem.NewStorage(dotGitFS, cache.NewObjectLRUDefault())

	res := &Repo{
		rawURL: opts.URL,
		auth:   opts.Auth,
		storer: storer,
		fs:     diskFS,
		cookie: opts.GitCookies,
		tmpDir: tmpDir,
	}

	release, err := lockTransport(opts.GitCookies, azureDevOps(opts.URL))
	if err != nil {
		return false, fmt.Errorf("failed to set custom HTTPS client: %w", err)
	}
	defer release()

	cloneOpts := git.CloneOptions{
		RemoteName:      "origin",
		URL:             opts.URL,
		Auth:            opts.Auth,
		ReferenceName:   plumbing.NewBranchReferenceName(opts.Branch),
		SingleBranch:    true,
		InsecureSkipTLS: opts.Insecure,
	}

	// Azure DevOps requires multi_ack and multi_ack_detailed capabilities, which go-git doesn't
	// implement. But: it's possible to do a full clone by saying it's _not_ _un_supported, in which
	// case the library happily functions so long as it doesn't _actually_ get a multi_ack packet. See
	// https://github.com/go-git/go-git/blob/v5.5.1/_examples/azure_devops/main.go.
	//
	// transport.UnsupportedCapabilities is a process-global as well, so only read or write it when
	// we actually need the workaround; lockTransport gave us the exclusive lock in that case.
	if azureDevOps(opts.URL) {
		oldUnsupportedCaps := transport.UnsupportedCapabilities
		defer restoreUnsupportedCapabilities(oldUnsupportedCaps)
		transport.UnsupportedCapabilities = []capability.Capability{
			capability.ThinPack,
		}
	}

	res.repo, err = git.Clone(res.storer, res.fs, &cloneOpts)
	if err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			log.Warn("Branch not found in remote repository", "branch", opts.Branch, "url", opts.URL)
			return false, nil
		}
		return false, fmt.Errorf("failed to clone repository: %w", normalizeEmptyReasonError(err))
	}
	head, err := res.repo.Head()
	if err != nil {
		return false, fmt.Errorf("failed to get HEAD: %v", err)
	}
	iter, err := res.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return false, fmt.Errorf("failed to get commit history: %v", err)
	}

	// Iterate through the commits
	found := false
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash.String() == hash {
			found = true
			return nil
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to iterate through commits: %v", err)
	}
	return found, err
}

// the first return value is false if a git-provider commit is
func IsFuncInGitCommitHistory(ctx context.Context, opts ListOptions, f func(commit *object.Commit) bool) (plumbing.Hash, error) {

	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	tmpDir, err := os.MkdirTemp(opts.HomeDir, "git-provider-history-*")
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	diskFS := osfs.New(tmpDir)
	dotGitFS, err := diskFS.Chroot(".git")
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to create .git directory: %w", err)
	}

	storer := filesystem.NewStorage(dotGitFS, cache.NewObjectLRUDefault())

	res := &Repo{
		rawURL: opts.URL,
		auth:   opts.Auth,
		storer: storer,
		fs:     diskFS,
		cookie: opts.GitCookies,
		tmpDir: tmpDir,
	}

	release, err := lockTransport(opts.GitCookies, azureDevOps(opts.URL))
	if err != nil {
		return plumbing.Hash{}, err
	}
	defer release()

	cloneOpts := git.CloneOptions{
		RemoteName:      "origin",
		URL:             opts.URL,
		Auth:            opts.Auth,
		ReferenceName:   plumbing.NewBranchReferenceName(opts.Branch),
		SingleBranch:    true,
		InsecureSkipTLS: opts.Insecure,
	}

	// Azure DevOps requires multi_ack and multi_ack_detailed capabilities, which go-git doesn't
	// implement. But: it's possible to do a full clone by saying it's _not_ _un_supported, in which
	// case the library happily functions so long as it doesn't _actually_ get a multi_ack packet. See
	// https://github.com/go-git/go-git/blob/v5.5.1/_examples/azure_devops/main.go.
	//
	// transport.UnsupportedCapabilities is a process-global as well, so only read or write it when
	// we actually need the workaround; lockTransport gave us the exclusive lock in that case.
	if azureDevOps(opts.URL) {
		oldUnsupportedCaps := transport.UnsupportedCapabilities
		defer restoreUnsupportedCapabilities(oldUnsupportedCaps)
		transport.UnsupportedCapabilities = []capability.Capability{
			capability.ThinPack,
		}
	}

	res.repo, err = git.Clone(res.storer, res.fs, &cloneOpts)
	if err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			log.Warn("Branch not found in remote repository", "branch", opts.Branch, "url", opts.URL)
			return plumbing.Hash{}, nil
		}
		return plumbing.Hash{}, fmt.Errorf("failed to clone repository: %w", normalizeEmptyReasonError(err))
	}
	head, err := res.repo.Head()
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to get HEAD: %v", err)
	}
	iter, err := res.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to get commit history: %v", err)
	}

	// Iterate through the commits
	found := plumbing.Hash{}
	err = iter.ForEach(func(c *object.Commit) error {
		if f(c) {
			found = c.Hash
			return nil
		}
		return nil
	})
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to iterate through commits: %v", err)
	}
	return found, err
}

/*
The function simulate the application of filemode of each from the origin repo (contained in "IndexOption.FromPath") to the destination repo (to files contained in IndexOption.ToPath)
---- git update-index --chmod
*/
// UpdateIndex is a purely local worktree operation: it never opens a transport,
// so it must not touch the global transport registry.
func (s *Repo) UpdateIndex(idx *IndexOptions) error {
	getIndexRelative := func(basepath, targpath string) string {
		if len(basepath) > 0 && basepath[0] != '/' {
			basepath = fmt.Sprintf("%c%s", '/', basepath)
		}
		if len(targpath) > 0 && targpath[0] != '/' {
			targpath = fmt.Sprintf("%c%s", '/', targpath)
		}
		path, err := filepath.Rel(basepath, targpath)
		if err != nil {
			return targpath
		}
		if path == "." {
			return ""
		}
		return path
	}

	fromIdx, err := idx.OriginRepo.storer.Index()
	if err != nil {
		return err
	}
	toIdx, err := s.storer.Index()
	if err != nil {
		return err
	}
	pattern := path.Join(getIndexRelative("/", idx.ToPath), "*")
	subInd, err := toIdx.Glob(pattern)
	if err != nil {
		return err
	}
	for _, e := range subInd {
		relativeName := getIndexRelative(idx.ToPath, e.Name)
		relativeSrc := getIndexRelative("/", idx.FromPath)

		/* .Entry() return ErrEntryNotFound if there is no match.
		The error is ignored because the destination folder can contain element that are not included in the source repo */
		fromEntry, _ := fromIdx.Entry(path.Join(relativeSrc, relativeName))

		//if Entry doesn't return an element skip to the next without updating
		if fromEntry != nil {
			e.Mode = fromEntry.Mode
		}
	}
	return nil
}
func Clone(opts CloneOptions) (*Repo, error) {
	return clone(context.Background(), opts)
}

func clone(ctx context.Context, opts CloneOptions) (*Repo, error) {
	tmpDir, err := os.MkdirTemp(opts.HomeDir, "git-provider-clone-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}

	diskFS := osfs.New(tmpDir)

	dotGitFS, err := diskFS.Chroot(".git")
	if err != nil {
		return nil, fmt.Errorf("failed to create .git directory: %w", err)
	}

	storer := filesystem.NewStorage(dotGitFS, cache.NewObjectLRUDefault())
	res := &Repo{
		rawURL: opts.URL,
		auth:   opts.Auth,
		storer: storer,
		fs:     diskFS,
		cookie: opts.GitCookies,
		tmpDir: tmpDir,
	}

	// One lock for the whole clone, covering the nested getLatestCommitRemote call below.
	mutatesGlobals := opts.UnsupportedCapabilities || azureDevOps(opts.URL)
	release, err := lockTransport(opts.GitCookies, mutatesGlobals)
	if err != nil {
		return nil, err
	}
	defer release()

	// Azure DevOps requires multi_ack and multi_ack_detailed capabilities, which go-git doesn't
	// implement. But: it's possible to do a full clone by saying it's _not_ _un_supported, in which
	// case the library happily functions so long as it doesn't _actually_ get a multi_ack packet. See
	// https://github.com/go-git/go-git/blob/v5.5.1/_examples/azure_devops/main.go.
	//
	// Restore the previous value on the way out instead of leaking this process-global to every
	// later clone, the way the old code did.
	if mutatesGlobals {
		oldUnsupportedCaps := transport.UnsupportedCapabilities
		defer restoreUnsupportedCapabilities(oldUnsupportedCaps)
		transport.UnsupportedCapabilities = []capability.Capability{
			capability.ThinPack,
		}
	}

	// Clone the given repository to the given directory
	cloneOpts := git.CloneOptions{
		RemoteName:      "origin",
		URL:             opts.URL,
		Auth:            opts.Auth,
		ReferenceName:   plumbing.NewBranchReferenceName(opts.Branch),
		SingleBranch:    true,
		InsecureSkipTLS: opts.Insecure,
	}
	isOrphan := true
	_, err = getLatestCommitRemote(ListOptions{
		URL:        opts.URL,
		Auth:       opts.Auth,
		Insecure:   opts.Insecure,
		Branch:     opts.Branch,
		GitCookies: opts.GitCookies,
	})
	if err != nil {
		if !errors.Is(err, ErrBranchNotFound) {
			return nil, fmt.Errorf("failed to inspect remote branch: %w", normalizeEmptyReasonError(err))
		}
		cloneOpts = git.CloneOptions{
			RemoteName:      "origin",
			URL:             opts.URL,
			Auth:            opts.Auth,
			InsecureSkipTLS: opts.Insecure,
		}
		if opts.AlternativeBranch != nil && len(ptr.Deref(opts.AlternativeBranch, "")) > 0 {
			isOrphan = false
			cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(ptr.Deref(opts.AlternativeBranch, ""))
			cloneOpts.SingleBranch = true
		}
		res.isNewBranch = ptr.To(true)
	}
	res.repo, err = git.Clone(res.storer, res.fs, &cloneOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to clone repository: %w", normalizeEmptyReasonError(err))
	}

	err = res.Branch(opts.Branch, &CreateOpt{
		Create: ptr.Deref(res.isNewBranch, false),
		Orphan: isOrphan,
	})

	return res, err
}

func IsInGitCommitHistory(opts ListOptions, hash string) (bool, error) {
	return isInGitCommitHistory(context.Background(), opts, hash)
}

func IsInGitCommitHistoryContext(ctx context.Context, opts ListOptions, hash string) (bool, error) {
	return isInGitCommitHistory(ctx, opts, hash)
}

// Exists stats the local filesystem: it never opens a transport, so it must not
// touch the global transport registry.
func (s *Repo) Exists(path string) (bool, error) {
	_, err := s.fs.Stat(path)
	if err != nil {
		if utils.IsErr(ErrRepositoryNotFound, err) {
			return false, ErrRepositoryNotFound
		}

		return false, err
	}

	return true, nil
}

func (s *Repo) FS() billy.Filesystem {
	return s.fs
}

func (s *Repo) Cleanup() error {
	if s.tmpDir != "" {
		return os.RemoveAll(s.tmpDir)
	}
	return nil
}

// CurrentBranch reads a local ref: it never opens a transport, so it must not
// touch the global transport registry.
func (s *Repo) CurrentBranch() string {
	//head, _ := s.repo.Head()
	head, _ := s.repo.Reference(plumbing.HEAD, false)

	return head.Target().Short()
}

type CreateOpt struct {
	Create bool
	Orphan bool
}

/*
Switch braches or create according to parameters passed in createOpt.
  - if createOpt is `nil` no branch are created and a `git checkout` is performed on branch specified by name
  - if creteOpt is different from nil and createOpt.Create is true a new branch is created checking out from the branch specified during clone - `git checkout -b branch-name`
  - if creteOpt is different from nil and both createOpt.Create and createOpt.Orphan are true a new branch is created from blank with no history or parents - `git switch --orphan branch-name`
*/
// Branch is a purely local operation on refs and the worktree: it never opens a
// transport, so it must not touch the global transport registry. clone calls it
// while holding the transport lock, which a lock here would deadlock against.
func (s *Repo) Branch(name string, createOpt *CreateOpt) error {
	ref := plumbing.NewBranchReferenceName(name)
	if createOpt != nil && createOpt.Create {
		ref = plumbing.NewBranchReferenceName(name)
		wt, err := s.repo.Worktree()
		if err != nil {
			return err
		}
		if createOpt.Orphan {
			if err := s.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref)); err != nil {
				return err
			}
			// Remove all files in the worktree
			if err := wt.RemoveGlob("*"); err != nil {
				return err
			}
			return err
		}

		return wt.Checkout(&git.CheckoutOptions{
			Create: true,
			Branch: ref,
		})
	}

	h := plumbing.NewSymbolicReference(plumbing.HEAD, ref)
	err := s.repo.Storer.SetReference(h)
	if err != nil {
		return err
	}

	wt, err := s.repo.Worktree()
	if err != nil {
		return err
	}

	return wt.Checkout(&git.CheckoutOptions{
		Create: false,
		Branch: ref,
	})
}

// Commit is a purely local worktree operation: it never opens a transport, so it
// must not touch the global transport registry.
func (s *Repo) Commit(path, msg string, opt *IndexOptions) (plumbing.Hash, error) {
	wt, err := s.repo.Worktree()
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to get worktree: %w", err)
	}

	// git add $path
	if _, err := wt.Add(path); err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to add file to index: %w", err)
	}

	if opt.OriginRepo != nil {
		err = s.UpdateIndex(opt)
		if err != nil {
			return plumbing.Hash{}, fmt.Errorf("failed to update index: %w", err)
		}
	}

	fStatus, err := wt.Status()
	if err != nil {
		return plumbing.Hash{}, fmt.Errorf("failed to get status of worktree: %w", err)
	}

	if fStatus.IsClean() && !ptr.Deref(s.isNewBranch, false) {
		return plumbing.Hash{}, NoErrAlreadyUpToDate
	}

	// git commit -m $message
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  CommitAuthorName,
			Email: CommitAuthorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return plumbing.Hash{}, NoErrAlreadyUpToDate
	}

	return hash, nil
}

func (s *Repo) Push(downstream, branch string, insecure bool) error {
	release, err := lockTransport(s.cookie, false)
	if err != nil {
		return err
	}
	defer release()

	//Push the code to the remote
	if len(branch) == 0 {
		return s.repo.Push(&git.PushOptions{
			RemoteName:      downstream,
			Auth:            s.auth,
			InsecureSkipTLS: insecure,
		})
	}
	refName := plumbing.NewBranchReferenceName(branch)

	refs, err := s.repo.References()
	if err != nil {
		return fmt.Errorf("failed to get references: %w", err)
	}

	var foundLocal bool
	refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() == refName {
			foundLocal = true
		}
		return nil
	})

	if !foundLocal {
		headRef, err := s.repo.Head()
		if err != nil {
			return fmt.Errorf("failed to get HEAD reference: %w", err)
		}

		ref := plumbing.NewHashReference(refName, headRef.Hash())
		err = s.repo.Storer.SetReference(ref)
		if err != nil {
			return fmt.Errorf("failed to create local branch reference: %w", err)
		}
	}

	err = s.repo.Push(&git.PushOptions{
		RemoteName:      downstream,
		Force:           false,
		Auth:            s.auth,
		InsecureSkipTLS: insecure,
		RefSpecs: []config.RefSpec{
			config.RefSpec(refName + ":" + refName),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to push to remote: %w", err)
	}
	return nil
}

func Pull(s *Repo, insecure bool) error {
	release, err := lockTransport(s.cookie, false)
	if err != nil {
		return err
	}
	defer release()

	// Get the working directory for the repository
	wt, err := s.repo.Worktree()
	if err != nil {
		return err
	}

	err = wt.Pull(&git.PullOptions{
		RemoteName:      "origin",
		Auth:            s.auth,
		InsecureSkipTLS: insecure,
	})

	if err != nil {
		if utils.IsErr(git.NoErrAlreadyUpToDate, err) {
			err = nil
		}
	}

	return err
}

// GetLatestCommit reads a local ref: it never opens a transport, so it must not
// touch the global transport registry.
func (s *Repo) GetLatestCommit(branch string) (string, error) {
	refName := plumbing.NewBranchReferenceName(branch)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}
