package repo

import (
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/templating"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
)

// render drives the real copier, so these assert what a Repo would actually commit.
func render(t *testing.T, annotations map[string]string, values map[string]interface{}, body string) (string, error) {
	t.Helper()

	opts, err := templatingOptions(annotations, values)
	if err != nil {
		return "", err
	}

	src, dst := memfs.New(), memfs.New()
	f, err := src.Create("f.yaml")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	co, err := copier.NewCopier(src, dst,
		append([]copier.Option{copier.WithOriginCopyPath("/"), copier.WithTargetCopyPath("/")}, opts...)...)
	if err != nil {
		t.Fatalf("NewCopier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		return "", err
	}

	out, err := dst.Open("f.yaml")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer out.Close()
	buf := make([]byte, 8192)
	n, _ := out.Read(buf)
	return string(buf[:n]), nil
}

// A Repo with no configMapKeyRef templates nothing — unchanged, and the reason a chart copies
// cleanly today.
func TestRepoNoValuesCopiesVerbatim(t *testing.T) {
	body := "replicas: {{ .Values.replicas }}\ntenant: {% .tenant %}\n"
	got, err := render(t, nil, nil, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != body {
		t.Errorf("expected verbatim, got %q", got)
	}
}

// gotemplate on a Repo now behaves exactly as it does on a LocalResource: Helm syntax is
// untouched, and the Repo's own placeholders substitute.
func TestRepoGoTemplateCoexistsWithHelm(t *testing.T) {
	body := "name: {{ include \"x\" . }}\nreplicas: {{ .Values.replicas }}\ntenant: {% .tenant %}\n"
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineGoTemplate},
		map[string]interface{}{"tenant": "kiratech"}, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "name: {{ include \"x\" . }}\nreplicas: {{ .Values.replicas }}\ntenant: kiratech\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The undeclared-value scanner reaches the Repo path too.
func TestRepoGoTemplateCatchesUndeclaredValues(t *testing.T) {
	_, err := render(t, map[string]string{templating.Annotation: templating.EngineGoTemplate},
		map[string]interface{}{"tenant": "kiratech"}, "env: {% .environment %}\n")
	if err == nil {
		t.Fatal("expected the undeclared value to fail the sync")
	}
	if !strings.Contains(err.Error(), `no value declared for ".environment"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

// REGRESSION: "none" used to fall through to the else branch and silently get MUSTACHE — the
// opposite of what it asks for.
func TestRepoNoneIsHonouredNotMustache(t *testing.T) {
	body := "tenant: {{tenant}}\nreplicas: {{ .Values.replicas }}\n"
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineNone},
		map[string]interface{}{"tenant": "kiratech"}, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != body {
		t.Errorf("none must copy verbatim; mustache would have substituted. got %q", got)
	}
}

// REGRESSION: a typo used to switch engine silently.
func TestRepoUnknownEngineRejected(t *testing.T) {
	_, err := templatingOptions(map[string]string{templating.Annotation: "gotmeplate"},
		map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("expected an unrecognised engine to be rejected")
	}
	if !strings.Contains(err.Error(), "gotmeplate") {
		t.Errorf("the error should name the offending value, got: %v", err)
	}
}

// Mustache stays available, and this pins WHY it is documented as legacy: it uses `{{ }}`, so it
// eats Helm syntax, and a key it cannot resolve renders EMPTY rather than failing.
func TestRepoMustacheStillWorksAndShowsWhyItIsLegacy(t *testing.T) {
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineMustache},
		map[string]interface{}{"tenant": "kiratech"},
		"tenant: {{tenant}}\nreplicas: {{ .Values.replicas }}\n")
	if err != nil {
		t.Fatalf("mustache should still work: %v", err)
	}
	if !strings.Contains(got, "tenant: kiratech") {
		t.Errorf("mustache should substitute its own key, got %q", got)
	}
	if strings.Contains(got, ".Values.replicas") {
		t.Errorf("expected mustache to have eaten the Helm expression; got %q", got)
	}
	if !strings.Contains(got, "replicas: \n") {
		t.Errorf("expected the Helm expression to render EMPTY (silent), got %q", got)
	}
}

// Absent annotation keeps mustache, so existing Repos are unaffected.
func TestRepoDefaultRemainsMustache(t *testing.T) {
	got, err := render(t, nil, map[string]interface{}{"tenant": "kiratech"}, "tenant: {{tenant}}\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "tenant: kiratech\n" {
		t.Errorf("default engine changed; got %q", got)
	}
}

func TestRepoMalformedDelimsRejected(t *testing.T) {
	_, err := templatingOptions(map[string]string{
		templating.Annotation:       templating.EngineGoTemplate,
		templating.DelimsAnnotation: "oops",
	}, map[string]interface{}{"a": 1})
	if err == nil {
		t.Fatal("expected malformed delimiters to be rejected")
	}
}
