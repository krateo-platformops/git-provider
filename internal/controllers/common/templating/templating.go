// Package templating holds the one definition of how a resource asks for — or refuses —
// file templating.
//
// It exists because the repo and localresource controllers have twice shipped production bugs
// by disagreeing about a shared rule (which auth methods consume a username; when to install a
// templating pass). The annotation key and its accepted values live here so a second copy
// cannot drift from the first.
package templating

import (
	"fmt"
	"strings"

	commonapis "github.com/krateoplatformops/git-provider/apis/common/v1alpha1"
	"github.com/krateoplatformops/git-provider/internal/tools/copier"
	"github.com/krateoplatformops/git-provider/internal/tools/template"
)

// Annotation selects the templating engine applied to file content.
const Annotation = "krateo.io/templating-engine"

// DelimsAnnotation overrides the delimiters used for this resource's OWN placeholders, as
// "left,right" — e.g. "{{,}}" to opt back in to Go's native syntax. Absent means the default,
// which is `{% %}` precisely so it does not collide with Helm's `{{ }}`.
const DelimsAnnotation = "krateo.io/templating-delims"

const (
	// EngineGoTemplate renders through Go text/template + sprig, ALWAYS — including when the
	// resource declares no values. Choose it for content that templates without inputs, e.g.
	// `{{ now | date "2006-01-02" }}` or `{{ uuidv4 }}`.
	EngineGoTemplate = "gotemplate"

	// EngineMustache renders through mustache. Accepted by the repo controller.
	EngineMustache = "mustache"

	// EngineNone never renders. Choose it for authored content that must land byte-for-byte —
	// a Helm chart being the obvious case, since `{{ toYaml }}` and `{{ include }}` are Helm
	// builtins that Go text/template cannot resolve, and `{{ .Values.x }}` would render to
	// `<no value>` and be committed.
	EngineNone = "none"
)

// StatusErrors converts the copier's render failures into the API shape both controllers publish
// on their status. One definition, so the two cannot report the same thing differently.
func StatusErrors(errs []copier.RenderError) []commonapis.TemplatingError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]commonapis.TemplatingError, 0, len(errs))
	for _, e := range errs {
		out = append(out, commonapis.TemplatingError{Path: e.Path, Message: e.Err.Error()})
	}
	return out
}

// Delims reads DelimsAnnotation. An absent annotation yields the defaults. A malformed value is
// an error rather than a silent fallback: quietly using different delimiters than the author
// asked for would leave their placeholders unsubstituted in a committed file.
func Delims(annotations map[string]string) (left, right string, err error) {
	raw, ok := annotations[DelimsAnnotation]
	if !ok || strings.TrimSpace(raw) == "" {
		return template.DefaultLeftDelim, template.DefaultRightDelim, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed %s %q: expected \"left,right\" (e.g. %q)",
			DelimsAnnotation, raw, "{{,}}")
	}
	left, right = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if left == "" || right == "" {
		return "", "", fmt.Errorf("malformed %s %q: both delimiters must be non-empty", DelimsAnnotation, raw)
	}
	return left, right, nil
}
