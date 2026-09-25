package repo

import (
	"fmt"
)

// unreachableTargetCommitError reports a recorded target commit that the target branch can no
// longer reach, when spec.enableUpdate forbids doing anything about it.
//
// The wording is the point. The previous message — "target commit X not found on branch main while
// enableUpdate is false" — describes what the check found, and reads as though the commit was
// pinned wrong. Usually it was not: the commit was discarded along with the history that contained
// it, when the target repository was recreated or force-pushed. Nine Repo CRs hit this at once on a
// single such event, and the message sent the reader looking for nine separate bad pins (#23).
//
// It stays an error. There is a real decision here for a person to make, so reporting it until they
// make it is correct — unlike #22, where the provider refused on an ambiguity nothing would ever
// resolve and the resource was stranded with no path back.
//
// What the message must carry: which commit and target (so the resource is identifiable), the CAUSE
// (so the reader does not go hunting for a mistake that was not made), and both remedies. Naming
// only enableUpdate would push operators toward allowing overwrites they may not want.
func unreachableTargetCommitError(commitID, branch, toRepoURL string) error {
	return fmt.Errorf(
		"target commit %s is no longer reachable from branch %s of %s, and spec.enableUpdate is false. "+
			"This usually means the target repository was recreated or its history rewritten, which "+
			"discards the commit rather than invalidating it — it does not mean the commit was pinned "+
			"wrong. Set enableUpdate to true to let the provider re-push, or recreate this resource to "+
			"pin the current history.",
		commitID, branch, toRepoURL)
}
