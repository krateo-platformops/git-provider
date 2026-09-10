package copier

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
)

// A copy must report EVERY file it could not render, not just the first. Go's text/template
// positions errors against an anonymous template ("template: template:61: ..."), so without the
// path an operator copying a tree cannot tell which file is at fault.
func TestCopyCollectsEveryRenderFailureWithItsPath(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "templates/a.yaml", "x: {{ toYaml .foo }}\n")   // Helm builtin
	writeFile(t, src, "templates/b.yaml", "y: {{ include \"z\" . }}\n") // Helm builtin
	writeFile(t, src, "values.yaml", "replicas: 1\n")                   // fine

	co, err := NewCopier(src, dst,
		WithOriginCopyPath("/"), WithTargetCopyPath("/"),
		WithGoTemplateDelims(nil, "{{", "}}"))
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}

	err = co.Copy(true)
	if err == nil {
		t.Fatal("expected the copy to fail")
	}

	var failed *RenderFailed
	if !errors.As(err, &failed) {
		t.Fatalf("expected a *RenderFailed, got %T: %v", err, err)
	}
	if len(failed.Errors) != 2 {
		t.Fatalf("expected 2 failures, got %d: %+v", len(failed.Errors), failed.Errors)
	}

	paths := map[string]bool{}
	for _, re := range failed.Errors {
		paths[re.Path] = true
		if re.Err == nil {
			t.Errorf("%s: nil error recorded", re.Path)
		}
	}
	for _, want := range []string{"/templates/a.yaml", "/templates/b.yaml"} {
		if !paths[want] {
			t.Errorf("missing %s; got %v", want, paths)
		}
	}

	// The aggregate message names the count and the paths, so the condition message is useful
	// even before anyone looks at status.templatingErrors.
	msg := failed.Error()
	for _, want := range []string{"2 files", "a.yaml", "b.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregate message missing %q: %s", want, msg)
		}
	}

	// RenderErrors() exposes the same list to the controllers.
	if len(co.RenderErrors()) != 2 {
		t.Errorf("RenderErrors() = %d, want 2", len(co.RenderErrors()))
	}
}

// A single failure keeps the message readable rather than pluralising awkwardly.
func TestSingleRenderFailureMessageNamesTheFile(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "only.yaml", "x: {{ toYaml .foo }}\n")

	co, _ := NewCopier(src, dst, WithOriginCopyPath("/"), WithTargetCopyPath("/"),
		WithGoTemplateDelims(nil, "{{", "}}"))
	err := co.Copy(true)
	if err == nil || !strings.Contains(err.Error(), "only.yaml") {
		t.Fatalf("expected the message to name the file, got: %v", err)
	}
}

// A clean copy leaves no residue for the next one to report.
func TestRenderErrorsResetBetweenCopies(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "bad.yaml", "x: {{ toYaml .foo }}\n")
	co, _ := NewCopier(src, dst, WithOriginCopyPath("/"), WithTargetCopyPath("/"),
		WithGoTemplateDelims(nil, "{{", "}}"))
	if err := co.Copy(true); err == nil {
		t.Fatal("expected failure")
	}
	if len(co.RenderErrors()) != 1 {
		t.Fatalf("setup: want 1 error, got %d", len(co.RenderErrors()))
	}

	// Same copier, a source that renders cleanly.
	src2 := memfs.New()
	writeFile(t, src2, "good.yaml", "replicas: 1\n")
	co.fromFS = src2
	if err := co.Copy(true); err != nil {
		t.Fatalf("second copy should succeed: %v", err)
	}
	if got := len(co.RenderErrors()); got != 0 {
		t.Errorf("errors from the previous copy leaked: %d", got)
	}
}
