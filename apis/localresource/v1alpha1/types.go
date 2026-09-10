package v1alpha1

import (
	commonapis "github.com/krateoplatformops/git-provider/apis/common/v1alpha1"
	prv1 "github.com/krateoplatformops/provider-runtime/apis/common/v1"
	"github.com/krateoplatformops/provider-runtime/pkg/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type Credentials struct {
	// AuthMethod: Possible values are: `basic`, `bearer`, `cookiefile`. `basic` requires  `secretRef` and `usernameRef`; `basic` requires only `secretRef`; `cookiefile` requires only `secretRef`
	// In case of 'cookiefile' the secretRef must contain a file with the cookie.
	// +kubebuilder:validation:Enum=basic;bearer;cookiefile
	// +kubebuilder:default:=basic
	// +optional
	AuthMethod string `json:"authMethod,omitempty"`

	// SecretRef: reference to a secret that contains token required to git server authentication or cookie file in case of 'cookiefile' authMethod.
	SecretRef *prv1.SecretKeySelector `json:"secretRef"`

	// UsernameRef: holds username required to git server authentication. - If 'authMethod' is 'bearer' or 'cookiefile' the field is ignored.
	// +optional
	UsernameRef *prv1.SecretKeySelector `json:"usernameRef"`
}
type LocalResourceOpts struct {
	// Url: url of the remote repository
	Url string `json:"url"`

	// Path: if in spec.fromLocalResource, Represents the folder to clone from. If not set the entire repository is cloned. If in spec.toLocalResource, represents the folder to use as destination.
	// +kubebuilder:default:="/"
	// +optional
	Path string `json:"path,omitempty"`

	// Branch: if in spec.fromLocalResource, the branch to copy from. If in spec.toLocalResource, represents the branch to populate; If the branch does not exist on remote is created by the provider.
	// +required
	Branch string `json:"branch"`

	// Credentials: Credentials required for git server authentication.
	// +required
	Credentials Credentials `json:"credentials,omitempty"`
	/*
		CloneFromBranch: used the parent of the new branch.
		- If the branch exists, the parameter is ignored.
		- If the parameter is not set, the branch is created empty and has no parents (no history) - `git switch --orphan branch-name`
	*/
	// +optional
	CloneFromBranch string `json:"cloneFromBranch,omitempty"`
}

type ResourceRef struct {
	// Name: Name of the resource to reference
	// +required
	Name string `json:"name"`

	// Namespace: Namespace of the resource to reference. If not set, the resource is expected to be cluster-scoped.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ApiVersion: API version of the resource to reference
	// +required
	ApiVersion string `json:"apiVersion"`

	// Resource: Kind of the resource to reference
	// +required
	Resource string `json:"resource"`
}

type Placeholder struct {
	// Name: Name of the placeholder to replace in the format {{ .name }}
	// +required
	Name string `json:"name"`

	// Value: Value to replace the placeholder with
	// +required
	Value string `json:"value"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.fromString) || has(self.fileName)",message="fileName is required when fromString is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.fromRef) || self.fromRef == oldSelf.fromRef",message="fromRef is immutable once set"
// +kubebuilder:validation:ExactlyOneOf=fromYaml;fromRef;fromString
type FromResource struct {
	// FileName: represent the file name to use. If not set the filename is defined as "gvk"_"metadata.name"_"metadata.namespace".yaml
	// +kubebuilder:validation:Pattern=`^[\w\-. {}]+$`
	// +optional
	FileName string `json:"fileName,omitempty"`

	// FromYaml: Valid K8s Manifest to copy from. It is considered valid if it contains Kind, APIVersion and (Name or GenerateName) fields.
	// +kubebuilder:validation:EmbeddedResource
	// +optional
	FromYaml *runtime.RawExtension `json:"fromYaml,omitempty"`

	// FromRef: Reference to a resource in the same cluster to copy from
	// +optional
	FromRef *ResourceRef `json:"fromRef,omitempty"`

	// FromString: This can cointain a string representation of a K8s manifest to copy from or any other text content. This is not validated but it preserves the complete ordering of fields as in the original string.
	// +optional
	FromString *string `json:"fromString,omitempty"`
}

// A LocalResourceSpec defines the desired state of a LocalResource.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.toRepo) || self.toRepo == oldSelf.toRepo",message="toRepo is immutable once set"
// +kubebuilder:validation:XValidation:rule="self.syncEnabled || self.override || !has(oldSelf.placeholdersToOverride) || self.placeholdersToOverride == oldSelf.placeholdersToOverride",message="placeholdersToOverride is immutable when syncEnabled is false"
// +kubebuilder:validation:XValidation:rule="self.syncEnabled || self.override || !has(oldSelf.fromResource.fromYaml) || self.fromResource.fromYaml == oldSelf.fromResource.fromYaml",message="fromResource.fromYaml is immutable when syncEnabled is false"
// +kubebuilder:validation:XValidation:rule="self.syncEnabled || self.override || !has(oldSelf.fromResource.fromString) || self.fromResource.fromString == oldSelf.fromResource.fromString",message="fromResource.fromString is immutable when syncEnabled is false"
// +kubebuilder:validation:XValidation:rule="!self.toRepo.url.contains('dev.azure.com') || self.unsupportedCapabilities == true",message="spec.unsupportedCapabilities must be true if toRepo.url contains 'dev.azure.com', as the go-git library does not support required capabilities for Azure DevOps. You can read more about that in the description of the 'unsupportedCapabilities' field."
type LocalResourceSpec struct {
	// PlaceholdersToOverride: List of placeholders to override in the format {{ .placeholder }}.
	// +optional
	PlaceholdersToOverride []Placeholder `json:"placeholdersToOverride,omitempty"`

	// FromResource: Resource to copy from
	// +required
	FromResource FromResource `json:"fromResource"`

	// ToRepo: Repository destination to copy to
	// +required
	ToRepo LocalResourceOpts `json:"toRepo"`

	// Insecure: Insecure is useful with hand made SSL certs (default: false)
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// UnsupportedCapabilities: If `true` [capabilities not supported by any client implementation](https://github.com/go-git/go-git/blob/4fd9979d5c2940e72bdd6946fec21e02d959f0f6/plumbing/transport/common.go#L310) will not be used by the provider
	// For azuredevops urls for example, the value must be true. Azure DevOps requires multi_ack and multi_ack_detailed capabilities, which go-git doesn't
	// implement. But: it's possible to do a full clone by saying it's _not_ _un_supported, in which
	// case the library happily functions so long as it doesn't _actually_ get a multi_ack packet. See
	// https://github.com/go-git/go-git/blob/v5.5.1/_examples/azure_devops/main.go.
	// +optional
	// +kubebuilder:default:=false
	UnsupportedCapabilities bool `json:"unsupportedCapabilities,omitempty"`

	// SyncEnabled: If `true`, the provider performs updates on the repository specified in `toLocalResource` when there are changes in `fromResource` or `fromResourceRef` field.
	// +kubebuilder:default:=false
	// +optional
	SyncEnabled bool `json:"syncEnabled,omitempty"`

	// CreateCommitMessage: Commit message to use when creating new files in the destination repository. A description will be appeded on the commit message indicating some information about the CR that have triggered the commit.
	// +optional
	// +kubebuilder:default:="chore: add files to remote repository"
	// +kubebuilder:validation:XValidation:rule="!self.contains('Managed by git-provider LocalResource:')",message="Il campo updateCommitMessage non può contenere la sottostringa 'Managed by git-provider LocalResource:'"
	CreateCommitMessage string `json:"createCommitMessage,omitempty"`

	// UpdateCommitMessage: Commit message to use when updating existing files in the destination repository. A description will be appeded on the commit message indicating some information about the CR that have triggered the commit.
	// +optional
	// +kubebuilder:default:="chore: update files in remote repository"
	// +kubebuilder:validation:XValidation:rule="!self.contains('Managed by git-provider LocalResource:')",message="Il campo updateCommitMessage non può contenere la sottostringa 'Managed by git-provider LocalResource:'"
	UpdateCommitMessage string `json:"updateCommitMessage,omitempty"`

	// Override: If `true`, the provider will override the existing files in the destination repository with the files from the source repository.
	// If `false`, the provider will only add new files and update existing files in the destination repository.
	// If not set, the provider will use the default behavior of adding new files.
	// Avoid using this option with originPath from / to /, as it will override also service folders like .git, .github, .gitignore, etc.
	// +kubebuilder:default:=false
	// +optional
	Override bool `json:"override,omitempty"`
}

// A LocalResourceStatus represents the observed state of a LocalResource.
type LocalResourceStatus struct {
	prv1.ConditionedStatus `json:",inline"`
	// TargetCommitId: last commit identifier of the target LocalResource
	TargetCommitId string `json:"targetCommitId,omitempty"`

	// TargetBranch: branch where commit was done
	TargetBranch string `json:"targetBranch,omitempty"`
	// TemplatingErrors: files whose content could not be rendered, with the renderer's own
	// message. Populated when a sync fails during templating and cleared on the next
	// successful one; a failed sync commits nothing, so this describes what would have been
	// published rather than what was.
	// +optional
	TemplatingErrors []commonapis.TemplatingError `json:"templatingErrors,omitempty"`
}

// +kubebuilder:object:root=true

// A LocalResource is a managed resource that represents a Krateo Git repository
// +kubebuilder:printcolumn:name="TARGET_COMMIT_ID",type="string",JSONPath=".status.targetCommitId"
// +kubebuilder:printcolumn:name="TARGET_BRANCH",type="string",JSONPath=".status.targetBranch"
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={git,krateo}
type LocalResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LocalResourceSpec   `json:"spec"`
	Status LocalResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LocalResourceList contains a list of LocalResource.
type LocalResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LocalResource `json:"items"`
}

// GetCondition of this LocalResource.
func (mg *LocalResource) GetCondition(ct prv1.ConditionType) prv1.Condition {
	return mg.Status.GetCondition(ct)
}

// SetConditions of this LocalResource.
func (mg *LocalResource) SetConditions(c ...prv1.Condition) {
	mg.Status.SetConditions(c...)
}

// GetItems of this LocalResourceList.
func (l *LocalResourceList) GetItems() []resource.Managed {
	items := make([]resource.Managed, len(l.Items))
	for i := range l.Items {
		items[i] = &l.Items[i]
	}
	return items
}
