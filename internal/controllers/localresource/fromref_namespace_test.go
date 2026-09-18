package localresource

import (
	"strings"
	"testing"
)

// krateo-platformops/git-provider#10: fromRef is resolved with the PROVIDER's ServiceAccount, which
// holds a cluster-wide get, and the fetched object is serialised in full and pushed to a git remote
// the same CR names. An author-chosen namespace therefore turned "may create a LocalResource" into
// "may read anything in the cluster, and publish it".
//
// This pins the confinement. The rule is kept pure precisely so it is testable — SyncLocalResources
// clones a repository long before it reaches this point.
func TestValidateFromRefNamespace(t *testing.T) {
	cases := []struct {
		name        string
		ref, cr     string
		wantErr     bool
		wantMention string
	}{
		{
			name: "same namespace is allowed",
			ref:  "team-a", cr: "team-a", wantErr: false,
		},
		{
			name: "empty namespace is allowed (cluster-scoped read path, not the escalation)",
			ref:  "", cr: "team-a", wantErr: false,
		},
		{
			name: "another namespace is REFUSED — this is the confused deputy",
			ref:  "kube-system", cr: "team-a", wantErr: true, wantMention: "kube-system",
		},
		{
			name: "krateo-system is not special-cased",
			ref:  "krateo-system", cr: "team-a", wantErr: true, wantMention: "krateo-system",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateFromRefNamespace(c.ref, c.cr)
			if c.wantErr != (err != nil) {
				t.Fatalf("validateFromRefNamespace(%q, %q) error = %v, wantErr %v", c.ref, c.cr, err, c.wantErr)
			}
			if !c.wantErr {
				return
			}
			// The operator has to be able to act on this, so the message must name the rejected
			// namespace and the one that was permitted — not just "forbidden".
			if !strings.Contains(err.Error(), c.wantMention) {
				t.Errorf("error must name the rejected namespace %q, got: %v", c.wantMention, err)
			}
			if !strings.Contains(err.Error(), c.cr) {
				t.Errorf("error must name the permitted namespace %q, got: %v", c.cr, err)
			}
		})
	}
}
