package template

import (
	"bytes"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

type TemplateValue struct {
	Key   string
	Value string
}

func FromTemplateValues(tplValues []TemplateValue) map[string]any {
	values := make(map[string]any)
	for _, v := range tplValues {
		values[v.Key] = v.Value
	}
	return values
}

// Delimiters.
//
// git-provider does NOT default to Go's `{{ }}`, and that is deliberate. The files it publishes
// are overwhelmingly Kubernetes manifests and Helm charts, and Helm owns `{{ }}`. Sharing the
// delimiter means a chart cannot be published with substitution at all: `{{ toYaml x }}` and
// `{{ include "y" . }}` fail (both are Helm builtins, not sprig ones), `{{ .Values.n }}` renders
// to the literal `<no value>`, and `{{/* ... */}}` headers — which `helm create` writes into
// every _helpers.tpl — are deleted outright.
//
// `{% %}` was chosen over the obvious alternatives by testing them against real YAML:
//
//	<< >>   breaks on the YAML merge key `<<: *defaults`   (parse error: expected :=)
//	[[ ]]   breaks on flow sequences `[[1,2],[3,4]]`       (parse error: unexpected ",")
//	{% %}   safe against both, and not Helm syntax
//
// Use GoLeftDelim/GoRightDelim to opt back in to `{{ }}` where the content is known not to be a
// chart.
const (
	DefaultLeftDelim  = "{%"
	DefaultRightDelim = "%}"

	GoLeftDelim  = "{{"
	GoRightDelim = "}}"
)

type Template string

// Render renders with git-provider's default delimiters.
func (t Template) Render(values map[string]any) ([]byte, error) {
	return t.RenderDelims(values, DefaultLeftDelim, DefaultRightDelim)
}

// RenderDelims renders with the given delimiters. Empty strings fall back to the defaults, so a
// caller that has nothing configured behaves like Render.
func (t Template) RenderDelims(values map[string]any, left, right string) ([]byte, error) {
	if left == "" || right == "" {
		left, right = DefaultLeftDelim, DefaultRightDelim
	}
	tpl, err := template.New("template").Delims(left, right).Funcs(sprig.FuncMap()).Parse(string(t))
	if err != nil {
		return nil, err
	}

	buf := bytes.Buffer{}
	if err := tpl.Execute(&buf, values); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
