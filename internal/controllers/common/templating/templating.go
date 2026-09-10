// Package templating holds the one definition of how a resource asks for — or refuses —
// file templating.
//
// It exists because the repo and localresource controllers have twice shipped production bugs
// by disagreeing about a shared rule (which auth methods consume a username; when to install a
// templating pass). The annotation key and its accepted values live here so a second copy
// cannot drift from the first.
package templating

// Annotation selects the templating engine applied to file content.
const Annotation = "krateo.io/templating-engine"

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
