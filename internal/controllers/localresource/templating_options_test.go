package localresource

import (
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/krateoplatformops/git-provider/internal/controllers/common/templating"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
	"github.com/krateoplatformops/git-provider/internal/tools/template"
)

// render exercises the options through the real copier, so these tests assert what actually
// lands in the target repo rather than which options were selected.
func render(t *testing.T, annotations map[string]string, values []template.TemplateValue, body string) (string, error) {
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
	buf := make([]byte, 4096)
	n, _ := out.Read(buf)
	return string(buf[:n]), nil
}

const chart = "replicas: {{ .Values.replicas }}\n"
const valueFree = `stamp: {% "x" | upper %}` + "\n"

// Default, no values: authored content survives. This is the sock-shop case.
func TestDefaultWithoutValuesCopiesVerbatim(t *testing.T) {
	got, err := render(t, nil, nil, chart)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != chart {
		t.Errorf("expected verbatim copy, got %q", got)
	}
}

// Default, values declared: substitution still happens. Unchanged behaviour.
func TestDefaultWithValuesSubstitutes(t *testing.T) {
	got, err := render(t, nil,
		[]template.TemplateValue{{Key: "name", Value: "sock-shop"}}, "project: {% .name %}\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "project: sock-shop\n" {
		t.Errorf("expected substitution, got %q", got)
	}
}

// gotemplate: render even with NO values — the value-free case (now, uuidv4, env, upper).
func TestGoTemplateRendersWithoutValues(t *testing.T) {
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineGoTemplate}, nil, valueFree)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "stamp: X\n" {
		t.Errorf("expected the value-free template to render, got %q", got)
	}
}

// none: never render, even when values ARE declared.
func TestNoneNeverRendersEvenWithValues(t *testing.T) {
	body := "project: {% .name %}\n"
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineNone},
		[]template.TemplateValue{{Key: "name", Value: "sock-shop"}}, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != body {
		t.Errorf("expected verbatim copy under %q, got %q", templating.EngineNone, got)
	}
}

// An unrecognised engine must fail loudly rather than silently picking a behaviour.
func TestUnknownEngineIsRejected(t *testing.T) {
	_, err := templatingOptions(map[string]string{templating.Annotation: "jinja2"}, nil)
	if err == nil {
		t.Fatal("expected an error for an unsupported engine, got nil")
	}
	if !strings.Contains(err.Error(), "jinja2") {
		t.Errorf("error should name the offending value, got: %v", err)
	}
}

// The point of the default delimiters: gotemplate no longer touches Helm syntax, so a chart can
// be published WITH substitution — the case that was impossible before.
func TestGoTemplateLeavesHelmSyntaxAloneByDefault(t *testing.T) {
	body := "name: {{ include \"x\" . }}\nreplicas: {{ .Values.replicas }}\ntenant: {% .tenant %}\n"
	got, err := render(t, map[string]string{templating.Annotation: templating.EngineGoTemplate},
		[]template.TemplateValue{{Key: "tenant", Value: "kiratech"}}, body)
	if err != nil {
		t.Fatalf("Helm syntax should no longer break the render: %v", err)
	}
	want := "name: {{ include \"x\" . }}\nreplicas: {{ .Values.replicas }}\ntenant: kiratech\n"
	if got != want {
		t.Errorf("expected Helm syntax preserved and {%% %%} substituted.\n got: %q\nwant: %q", got, want)
	}
}

// Opting back in to Go's native delimiters still works — and on a Helm chart it still fails,
// which is the honest outcome of asking for it.
func TestGoDelimsOptInStillCollidesWithHelm(t *testing.T) {
	_, err := render(t, map[string]string{
		templating.Annotation:       templating.EngineGoTemplate,
		templating.DelimsAnnotation: "{{,}}",
	}, nil, "command:\n  {{- toYaml $x | nindent 4 }}\n")
	if err == nil {
		t.Fatal("expected the Helm builtin to fail once {{ }} is opted into")
	}
	if !strings.Contains(err.Error(), `function "toYaml" not defined`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGoDelimsOptInSubstitutes(t *testing.T) {
	got, err := render(t, map[string]string{templating.DelimsAnnotation: "{{,}}"},
		[]template.TemplateValue{{Key: "name", Value: "sock-shop"}}, "project: {{ .name }}\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "project: sock-shop\n" {
		t.Errorf("expected substitution under opted-in {{ }}, got %q", got)
	}
}

func TestMalformedDelimsRejected(t *testing.T) {
	for _, bad := range []string{"{{", "a,b,c", ",}}", "{{,"} {
		if _, err := templatingOptions(map[string]string{templating.DelimsAnnotation: bad}, nil); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}
