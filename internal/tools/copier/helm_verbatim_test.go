package copier

import (
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	tpl "github.com/krateoplatformops/git-provider/internal/tools/template"
)

// A Helm chart template, as the Blueprint Builder publishes it. `toYaml` is a Helm
// builtin, not a sprig one, so text/template cannot render this at all.
const helmChart = `spec:
  containers:
    - name: {{ .Chart.Name }}
      command:
        {{- toYaml $svc.command | nindent 12 }}
      replicas: {{ .Values.replicas }}
`

// Without a templating pass the bytes must survive untouched — this is what a
// LocalResource with no placeholdersToOverride is asking for.
func TestCopyLeavesHelmTemplateVerbatimWhenNotTemplating(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "templates/microservices.yaml", helmChart)

	co, err := NewCopier(src, dst, WithOriginCopyPath("/"), WithTargetCopyPath("/"))
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := readFile(t, dst, "templates/microservices.yaml"); got != helmChart {
		t.Errorf("chart was not copied verbatim.\n got: %q\nwant: %q", got, helmChart)
	}
}

// The regression itself, now reachable only by opting in to Go's `{{ }}` delimiters: the same
// file fails with the exact error seen on krateo-057 for LocalResource/publish-sock-shop-003.
// With the DEFAULT delimiters this no longer happens at all — see
// TestGoTemplateLeavesHelmSyntaxAloneByDefault in the localresource package.
func TestGoTemplatePassRejectsHelmBuiltins(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "templates/microservices.yaml", helmChart)

	co, err := NewCopier(src, dst,
		WithOriginCopyPath("/"), WithTargetCopyPath("/"),
		WithGoTemplateDelims(nil, "{{", "}}"), // opted in to Go's delimiters, no values
	)
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}
	err = co.Copy(true)
	if err == nil {
		t.Fatal("expected the copy to fail on the Helm builtin, got nil")
	}
	if !strings.Contains(err.Error(), `function "toYaml" not defined`) {
		t.Errorf("expected the toYaml failure, got: %v", err)
	}
}

// The quieter half, and the reason this matters beyond one blocked publish: a template that
// merely PARSES used to be executed with a missing value committed as `<no value>` — no error,
// nothing in the logs, a corrupted file in the repo. It is now caught and the copy fails.
func TestUndeclaredValueIsCaughtRatherThanCommitted(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	const parseable = "replicas: {% .replicas %}\n"
	writeFile(t, src, "values.yaml", parseable)

	co, err := NewCopier(src, dst,
		WithOriginCopyPath("/"), WithTargetCopyPath("/"), WithGoTemplate(nil))
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}

	err = co.Copy(true)
	if err == nil {
		t.Fatal("expected the undeclared value to fail the copy, not render to <no value>")
	}
	if !strings.Contains(err.Error(), `no value declared for ".replicas"`) {
		t.Errorf("expected the undeclared value to be named, got: %v", err)
	}

	// The renderer alone would NOT have caught this — that is the whole point.
	out, rerr := tpl.Template(parseable).Render(map[string]any{})
	if rerr != nil {
		t.Fatalf("premise check: Render should succeed silently, got %v", rerr)
	}
	if !strings.Contains(string(out), "<no value>") {
		t.Fatalf("premise check: expected <no value>, got %q", string(out))
	}
}

// Substitution must still work when the CR does ask for it.
func TestPlaceholdersStillSubstituteWhenPresent(t *testing.T) {
	src, dst := memfs.New(), memfs.New()
	writeFile(t, src, "README.md", "project: {{ .name }}\n")

	co, err := NewCopier(src, dst,
		WithOriginCopyPath("/"), WithTargetCopyPath("/"),
		WithGoTemplateDelims([]tpl.TemplateValue{{Key: "name", Value: "sock-shop"}}, "{{", "}}"))
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got, want := readFile(t, dst, "README.md"), "project: sock-shop\n"; got != want {
		t.Errorf("placeholder not substituted: got %q want %q", got, want)
	}
}
