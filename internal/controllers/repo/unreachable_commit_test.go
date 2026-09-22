package repo

import (
	"strings"
	"testing"
)

// #23. The message must name the CAUSE and both remedies, not just the symptom.
//
// "target commit X not found on branch main while enableUpdate is false" reads as a bad pin. In the
// incident that prompted this, nothing was pinned wrong — one target repository was recreated,
// orphaning the recorded commits of nine resources at once. A message that implies the wrong remedy
// sends the reader to fix something that is not broken.
//
// The error itself is deliberately preserved: a person has to choose between re-pinning and
// allowing updates, and TC12/TC13 pin that contract.
func TestUnreachableTargetCommitErrorExplainsTheCause(t *testing.T) {
	const (
		commit = "131ceb6bc"
		branch = "main"
		url    = "https://github.com/krateo-blueprints/demo-service-catalog.git"
	)
	msg := unreachableTargetCommitError(commit, branch, url).Error()

	// Identifiers, so the operator knows which resource and which target.
	for _, want := range []string{commit, branch, url} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must identify %q, got: %s", want, msg)
		}
	}

	// The cause. Without it the reader assumes a mistaken pin.
	for _, want := range []string{"recreated", "history"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must explain the cause (%q), got: %s", want, msg)
		}
	}

	// Both remedies. Naming only the flag pushes operators toward allowing overwrites they may not
	// want; re-creating the resource re-pins to the current history instead.
	for _, want := range []string{"enableUpdate", "recreate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must offer the %q remedy, got: %s", want, msg)
		}
	}
}

// TC12 asserts the Synced message contains "target commit". That substring is load-bearing for the
// integration suite, and it is also what makes the error greppable across a fleet — nine resources
// failing for one reason should be findable as one string.
func TestUnreachableTargetCommitErrorKeepsTheGreppablePrefix(t *testing.T) {
	msg := unreachableTargetCommitError("abc123", "main", "https://example.com/repo.git").Error()

	if !strings.Contains(msg, "target commit") {
		t.Errorf("message must keep the 'target commit' substring: TC12 matches on it and it is how "+
			"this failure is found across a fleet. Got: %s", msg)
	}
}
