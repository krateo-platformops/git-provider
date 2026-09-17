package localresource

import (
	"errors"
	"fmt"
	"testing"
)

// The classifier decides what gets retried. Too broad and a genuine failure is retried until the
// backoff is exhausted, hiding the real error behind a timeout; too narrow and the wedge this fix
// exists to prevent comes straight back. Both directions are pinned here.
func TestIsRefContention(t *testing.T) {
	// Verbatim from git-provider on krateo-057 while seven LocalResources of one publish pushed to
	// the same branch. This exact string is the reason the retry exists.
	observed := errors.New("unable to push target LocalResource: failed to push to remote: " +
		"command error on refs/heads/builder/team-catalog: cannot lock ref " +
		"'refs/heads/builder/team-catalog': is at 6c72e11c82711731c9bdedd72 but expected c213724313")

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not contention", err: nil, want: false},
		{name: "the observed cannot-lock-ref failure", err: observed, want: true},
		{name: "wrapped, because callers wrap", err: fmt.Errorf("create failed: %w", observed), want: true},
		{name: "non-fast-forward", err: errors.New("failed to push: non-fast-forward update"), want: true},
		{name: "reference already exists", err: errors.New("reference already exists"), want: true},

		// Anything that will not be fixed by re-cloning must surface immediately.
		{name: "auth failure is NOT retried", err: errors.New("authentication required"), want: false},
		{name: "missing repo is NOT retried", err: errors.New("repository not found"), want: false},
		{name: "a clone failure is NOT retried", err: errors.New("cloning toLocalResource: no such host"), want: false},
		{name: "the ignore-path bug is NOT retried", err: errors.New(
			"failed to set krateo ignore: unable to open .krateoignore: not a directory"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRefContention(tc.err); got != tc.want {
				t.Fatalf("isRefContention(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The backoff must actually spread retries out. Siblings of one publish start within milliseconds of
// each other, so a zero-jitter or single-step backoff would have them collide again in lockstep.
func TestRefContentionRetryIsJitteredAndRepeats(t *testing.T) {
	if refContentionRetry.Steps < 3 {
		t.Fatalf("Steps = %d, want at least 3 — one retry does not survive several concurrent siblings",
			refContentionRetry.Steps)
	}
	if refContentionRetry.Jitter <= 0 {
		t.Fatal("Jitter must be non-zero, or siblings that started together retry in lockstep and collide again")
	}
	if refContentionRetry.Factor <= 1 {
		t.Fatalf("Factor = %v, want > 1 so the wait actually grows", refContentionRetry.Factor)
	}
}
