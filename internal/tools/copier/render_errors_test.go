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
	writeFile(t, src, "templates/a.yaml", "x: {{ toYaml .foo }}\n")     // Helm builtin
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
	// a.yaml has TWO problems (undefined function AND an undeclared value); b.yaml has one.
	// The scanner reports every problem, not one per file.
	byPath := map[string]int{}
	for _, re := range failed.Errors {
		byPath[re.Path]++
	}
	if byPath["/templates/a.yaml"] != 2 {
		t.Errorf("expected 2 problems in a.yaml, got %d: %+v", byPath["/templates/a.yaml"], failed.Errors)
	}
	if byPath["/templates/b.yaml"] != 1 {
		t.Errorf("expected 1 problem in b.yaml, got %d", byPath["/templates/b.yaml"])
	}
	if byPath["/values.yaml"] != 0 {
		t.Errorf("values.yaml renders cleanly and must not be reported")
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
	if got := len(failed.Files()); got != 2 {
		t.Errorf("Files() = %d, want 2 distinct paths", got)
	}

	msg := failed.Error()
	for _, want := range []string{"3 problems", "2 files", "a.yaml", "b.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregate message missing %q: %s", want, msg)
		}
	}

	// RenderErrors() exposes the same list to the controllers.
	if len(co.RenderErrors()) != 3 {
		t.Errorf("RenderErrors() = %d, want 3", len(co.RenderErrors()))
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
	if len(co.RenderErrors()) == 0 {
		t.Fatalf("setup: expected at least one error")
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
