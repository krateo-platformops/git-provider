// Package v1alpha1 holds types shared by every git-provider API group.
//
// It exists for the same reason internal/controllers/common/templating does: the repo and
// localresource controllers keep growing parallel definitions of the same concept, and every
// time they drift a defect ships.
//
// +kubebuilder:object:generate=true
package v1alpha1

// TemplatingError names a file whose content could not be rendered, and why.
//
// Templating failures used to surface only as a single wrapped error on the Synced condition,
// with no indication of WHICH file was at fault — Go's text/template reports positions against
// an anonymous template ("template: template:61: ..."), which is unusable when a sync copies
// more than one file.
type TemplatingError struct {
	// Path of the file, relative to the source copy path.
	Path string `json:"path"`

	// Message is the renderer's error, verbatim.
	Message string `json:"message"`
}
