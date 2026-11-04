// Package gittest builds throwaway git repositories on local disk for tests.
//
// The controllers' Observe reads remote history through the git client, which accepts a local
// path as a remote URL. A repository made here lets a test drive Observe down a chosen path with
// no network and no git server. go-git serves a local path by running the git-upload-pack binary,
// so git must be installed where the tests run.
package gittest

import (
	"os"
	"path/filepath"
	"strconv"
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
