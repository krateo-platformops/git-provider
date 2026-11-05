package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsInGitCommitHistory(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	opts := ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	}

	hash := "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"

	exists, err := IsInGitCommitHistory(context.Background(), opts, hash)
	if err != nil {
		t.Errorf("Error checking commit history: %v", err)
	}
	require.NoError(t, err)

	if exists {
		t.Logf("Commit %s exists in the Git repository", hash)
	} else {
		t.Errorf("Commit %s does not exist in the Git repository", hash)
	}
}

func TestGetLatestCommitRemote(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL: baseRepo.GetBasicLocalRepositoryURL(),
	})
	require.NoError(t, err)

	_, err = repo.FS().OpenFile("README.md", os.O_RDWR|os.O_CREATE, 0644)
	require.NoError(t, err)

	commit, err := GetLatestCommitRemote(context.Background(), ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	})
	require.NoError(t, err)
	expected := "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"
	assert.Equal(t, expected, *commit)

	commit, err = GetLatestCommitRemote(context.Background(), ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "branch",
	})
	require.NoError(t, err)
	expected = "e8d3ffab552895c19b9fcf7aa264d277cde33881"
	assert.Equal(t, expected, *commit)
}

func TestGetLatestCommitRemoteBranchNotFound(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	_, err := GetLatestCommitRemote(context.Background(), ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "missing-branch",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBranchNotFound))
	assert.Contains(t, err.Error(), "missing-branch")
}

func TestNormalizeEmptyReasonError(t *testing.T) {
	sentinel := errors.New("authentication required")
	err := fmt.Errorf("%w: ", sentinel)

	normalized := normalizeEmptyReasonError(err)

	require.Equal(t, "authentication required", normalized.Error())
	assert.True(t, errors.Is(normalized, sentinel))
}

func TestPull(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	})
	require.NoError(t, err)

	err = Pull(context.Background(), repo, false)
	require.NoError(t, err)
}

func TestBranch(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	})
	require.NoError(t, err)

	err = repo.Branch("test", &CreateOpt{
		Create: true,
		Orphan: false,
	})
	require.NoError(t, err)

	err = repo.Branch("test-orphan", &CreateOpt{
		Create: true,
		Orphan: true,
	})
	require.NoError(t, err)

	err = repo.Branch("test", nil)
	require.NoError(t, err)
}

func TestCurrentBranch(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	})
	require.NoError(t, err)

	branch := repo.CurrentBranch()
	assert.Equal(t, "master", branch)
}

func TestGetLatestCommit(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL: baseRepo.GetBasicLocalRepositoryURL(),
	})
	require.NoError(t, err)

	commit, err := repo.GetLatestCommit("master")
	require.NoError(t, err)
	expected := "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"
	assert.Equal(t, expected, commit)
}
func TestPush(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	t.Run("push without branch", func(t *testing.T) {
		repo, err := Clone(context.Background(), CloneOptions{
			URL:    baseRepo.GetBasicLocalRepositoryURL(),
			Branch: "master",
		})
		require.NoError(t, err)
		defer repo.Cleanup()

		// Create a new file with content
		file, err := repo.FS().OpenFile("test.txt", os.O_RDWR|os.O_CREATE, 0644)
		require.NoError(t, err)
		_, err = file.Write([]byte("test content"))
		require.NoError(t, err)
		err = file.Close()
		require.NoError(t, err)

		_, err = repo.Commit("test.txt", "Add test file", &IndexOptions{
			OriginRepo: repo,
			FromPath:   "/",
			ToPath:     "/",
		})
		require.NoError(t, err)

		// Push to a different branch to avoid the "currently checked out" error
		err = repo.Push(context.Background(), "origin", "test-branch", false)
		require.NoError(t, err)
	})

	t.Run("push with specific branch", func(t *testing.T) {
		repo, err := Clone(context.Background(), CloneOptions{
			URL:    baseRepo.GetBasicLocalRepositoryURL(),
			Branch: "master",
		})
		require.NoError(t, err)
		defer repo.Cleanup()

		// Create a new branch
		err = repo.Branch("feature", &CreateOpt{
			Create: true,
			Orphan: false,
		})
		require.NoError(t, err)

		// Create a new file with content
		file, err := repo.FS().OpenFile("feature.txt", os.O_RDWR|os.O_CREATE, 0644)
		require.NoError(t, err)
		_, err = file.Write([]byte("feature content"))
		require.NoError(t, err)
		err = file.Close()
		require.NoError(t, err)

		_, err = repo.Commit("feature.txt", "Add feature file", &IndexOptions{
			OriginRepo: repo,
			FromPath:   "/",
			ToPath:     "/",
		})
		require.NoError(t, err)

		err = repo.Push(context.Background(), "origin", "feature", false)
		require.NoError(t, err)
	})

	t.Run("push with insecure option", func(t *testing.T) {
		repo, err := Clone(context.Background(), CloneOptions{
			URL:    baseRepo.GetBasicLocalRepositoryURL(),
			Branch: "master",
		})
		require.NoError(t, err)
		defer repo.Cleanup()

		// Create a new file with content
		file, err := repo.FS().OpenFile("insecure_test.txt", os.O_RDWR|os.O_CREATE, 0644)
		require.NoError(t, err)
		_, err = file.Write([]byte("insecure test content"))
		require.NoError(t, err)
		err = file.Close()
		require.NoError(t, err)

		_, err = repo.Commit("insecure_test.txt", "Add insecure test file", &IndexOptions{
			OriginRepo: repo,
			FromPath:   "/",
			ToPath:     "/",
		})
		require.NoError(t, err)

		// Push to a different branch to avoid the "currently checked out" error
		err = repo.Push(context.Background(), "origin", "insecure-test-branch", true)
		require.NoError(t, err)
	})

	t.Run("push non-existing local branch", func(t *testing.T) {
		repo, err := Clone(context.Background(), CloneOptions{
			URL:    baseRepo.GetBasicLocalRepositoryURL(),
			Branch: "master",
		})
		require.NoError(t, err)
		defer repo.Cleanup()

		// Create a new file with content on master
		file, err := repo.FS().OpenFile("master_file.txt", os.O_RDWR|os.O_CREATE, 0644)
		require.NoError(t, err)
		_, err = file.Write([]byte("master file content"))
		require.NoError(t, err)
		err = file.Close()
		require.NoError(t, err)

		_, err = repo.Commit("master_file.txt", "Add master file", &IndexOptions{
			OriginRepo: repo,
			FromPath:   "/",
			ToPath:     "/",
		})
		require.NoError(t, err)

		// Try to push to a branch that doesn't exist locally yet
		err = repo.Push(context.Background(), "origin", "new-branch", false)
		require.NoError(t, err)
	})
}

func TestIsFuncInGitCommitHistory(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	opts := ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	}

	hash := "6ecf0ef2c2dffb796033e5a02219af86ec6584e5"

	h, err := IsFuncInGitCommitHistory(context.TODO(), opts, func(commit *object.Commit) bool {
		fmt.Println("Checking commit:", commit.Hash.String(), "with message:", commit.Message)
		return commit.Hash.String() == hash
	})
	if err != nil {
		t.Errorf("Error checking commit history: %v", err)
	}

	if !h.IsZero() {
		t.Logf("Commit %s exists in the Git repository", hash)
	} else {
		t.Errorf("Commit %s does not exist in the Git repository", hash)
	}
}

func TestIsFuncInGitCommitHistory_complex(t *testing.T) {
	baseRepo := BaseSuite{}
	baseRepo.BuildBasicRepository()

	repo, err := Clone(context.Background(), CloneOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	})
	require.NoError(t, err)
	defer repo.Cleanup()

	message := "Merge branch 'master' of github.com:tyba/git-fixture"
	commit := "1669dce138d9b841a518c64b10914d88f5e488ea"
	opts := ListOptions{
		URL:    baseRepo.GetBasicLocalRepositoryURL(),
		Branch: "master",
	}
	hash, err := IsFuncInGitCommitHistory(context.TODO(), opts, func(commit *object.Commit) bool {
		return strings.Contains(commit.Message, message)
	})
	require.NoError(t, err)

	require.Equal(t, commit, hash.String())

	require.False(t, hash.IsZero())

	// Try to push to a branch that doesn't exist locally yet
	err = repo.Push(context.Background(), "origin", "new-branch", false)
	require.NoError(t, err)
}
