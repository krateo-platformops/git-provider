# LocalResource

The `LocalResource` Custom Resource (CR) allows you to export content to a Git repository. The source content can be a Kubernetes manifest (either as a raw string or an embedded object) or a reference to an existing resource within the cluster.

## Overview

Unlike the `Repo` CR, which handles Git-to-Git cloning and templating, `LocalResource` focuses on the **K8s-to-Git** (or String-to-Git) workflow. It takes a defined source, optionally applies placeholder substitutions, and commits the result to a specified branch in a target Git repository.

**Why use it?**
*   **GitOps Backup/Export:** Automatically backup specific Kubernetes resources (like a `ConfigMap` or a dynamically generated `Secret`) directly into a Git repository. 
*   **Dynamic Documentation/Files:** Generate a `README.md`, an environment-specific configuration file, or any arbitrary file on the fly using the `fromString` capability and push it to a repository, injecting real-time cluster data via placeholders.

## Sources (`FromResource`)

You must define exactly one source type within the `spec.fromResource` block. 

### 1. `FromYaml`
Provides a valid Kubernetes manifest directly embedded in the CR. It is considered valid if it contains `kind`, `apiVersion`, and either `name` or `generateName`.

```yaml
fromResource:
  fromYaml:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: my-config
    data:
      key: value
```

### 2. `FromRef`
References an existing resource within the same cluster to copy from.

```yaml
fromResource:
  fromRef:
    apiVersion: v1
    resource: configmaps
    name: my-existing-config
    namespace: default # Optional, omit for cluster-scoped resources
```

### 3. `FromString`
Contains a string representation of a K8s manifest or any other text content. Unlike `fromYaml`, this is not strictly validated as a K8s object, preserving the exact string content and ordering. 

> [!IMPORTANT]  
> When using `fromString`, you **must** also provide a `fileName`.

```yaml
fromResource:
  fileName: custom-file.txt
  fromString: |
    This is some custom text content.
    It can be anything.
```

### File Name Generation
If `fileName` is not explicitly provided in `spec.fromResource` (and you are not using `fromString`), the provider automatically generates a filename based on the resource metadata:
`{kind}_{metadata.name}_{metadata.namespace}.yaml` (namespace is omitted if empty).

## Templating

`LocalResource` supports a simple placeholder replacement mechanism. You can define a list of `placeholdersToOverride` in the spec. The provider will look for occurrences of `{% .placeholderName %}` in the source content and replace them with the corresponding value.

```yaml
spec:
  placeholdersToOverride:
    - name: environment
      value: production
    - name: replicaCount
      value: "3"
```
In your source content, you would use `{% .environment %}` and `{% .replicaCount %}`.

Rendering is done with Go `text/template` plus the [sprig](https://masterminds.github.io/sprig/) function set.

### When templating is applied

Rendering is **not** a no-op on content that only looks like a template, so it is applied deliberately rather than always:

| `krateo.io/templating-engine` | behaviour |
|---|---|
| *(absent — the default)* | Render **only if** `placeholdersToOverride` is set. Otherwise the file is committed byte-for-byte. |
| `gotemplate` | Always render, including when no placeholders are declared. |
| `none` | Never render, even when placeholders are declared. |

Any other value is rejected with an error rather than silently treated as one of the above.

The default suits the two common cases at once: a resource that declares placeholders gets them substituted, and a resource that declares none is copied verbatim.

### Delimiters

Placeholders use **`{% %}`**, not Go's usual `{{ }}`.

That is deliberate. The files this provider publishes are overwhelmingly Kubernetes manifests and Helm charts, and **Helm owns `{{ }}`**. Sharing the delimiter makes it impossible to publish a chart with substitution at all — `{{ toYaml x }}` and `{{ include "y" . }}` fail (both are Helm builtins, not sprig ones), `{{ .Values.n }}` renders to the literal `<no value>`, and the `{{/* ... */}}` header `helm create` writes into every `_helpers.tpl` is deleted.

With `{% %}` the two coexist — each system renders its own half, at its own time:

```yaml
metadata:
  labels:
    krateo.io/tenant: {% .tenant %}              # substituted at publish time, by this provider
spec:
  replicas: {{ .Values.replicas }}               # left alone; rendered by Helm at install time
  containers:
    {{- toYaml .Values.containers | nindent 4 }} # left alone
```

`{% %}` was chosen by testing the alternatives against real YAML: `<< >>` breaks on the merge key `<<: *defaults`, and `[[ ]]` breaks on flow sequences such as `[[1,2],[3,4]]`.

Override with `krateo.io/templating-delims: "left,right"` — for example `"{{,}}"` to use Go's native syntax where the content is known not to be a chart. A malformed value is rejected rather than silently ignored.

Set `gotemplate` when the content templates **without inputs** — sprig functions that need no values:

```yaml
metadata:
  annotations:
    krateo.io/templating-engine: gotemplate
spec:
  fromResource:
    fileName: README.md
    fromString: |
      Generated on {{ now | date "2006-01-02" }} — build {{ uuidv4 }}.
```

Set `none` for authored content that must survive exactly, most often a **Helm chart**. Chart templates are rendered by Helm at install time, not by this provider at commit time, and pushing them through Go `text/template` first either fails or corrupts them:

* `{{ toYaml ... }}`, `{{ include ... }}`, `{{ required ... }}` and `{{ tpl ... }}` are **Helm** builtins, not sprig ones — the sync fails with `function "toYaml" not defined`.
* `{{ .Values.replicas }}` is worse, because it *parses*: it renders against an empty value set and is committed as the literal string `<no value>`.
* `{{/* ... */}}` comment headers — which `helm create` writes at the top of every `_helpers.tpl` — are removed.

```yaml
metadata:
  annotations:
    krateo.io/templating-engine: none
```

With the default (no annotation and no placeholders) a chart is already copied verbatim; `none` states the intent explicitly and keeps holding if placeholders are added later.


### When templating fails

A render failure names the file. `status.templatingErrors` lists **every** file that failed, not just the first, because Go reports template positions against an anonymous template (`template: template:61: ...`) which is useless when a sync copies more than one file:

```yaml
status:
  conditions:
    - type: Synced
      status: "False"
      reason: ReconcileError
      message: 'unable to copy files: rendering failed for 2 files: templates/a.yaml, templates/b.yaml (first: function "toYaml" not defined)'
  templatingErrors:
    - path: templates/a.yaml
      message: 'template: template:1: function "toYaml" not defined'
    - path: templates/b.yaml
      message: 'template: template:1: function "include" not defined'
```

A failed sync commits nothing, so this describes what *would* have been published. The list is cleared on the next successful sync.

## Custom Commits

You can customize the commit messages used when the provider pushes changes to the target repository. A description indicating the CR that triggered the commit will be automatically appended to these messages.

*   `createCommitMessage`: Used when creating new files (Default: `"chore: add files to remote repository"`).
*   `updateCommitMessage`: Used when updating existing files (Default: `"chore: update files in remote repository"`).

> [!NOTE]
> You cannot include the substring `"Managed by git-provider LocalResource:"` in your custom messages, as this is reserved for the provider's internal tracking.

## Synchronization & Overrides

The behavior of how `LocalResource` interacts with the target repository over time is controlled by two key flags: `syncEnabled` and `override`.

### `syncEnabled`
*   **`false` (Default):** The provider performs a "one-shot" execution. It will process the source and push it to the target repository once. Subsequent changes to the source resource (e.g., modifying the `ConfigMap` referenced by `fromRef`) will **not** trigger a new commit. Furthermore, the source definitions (`fromYaml`, `fromString`, `placeholdersToOverride`) become immutable.
*   **`true`:** The provider continuously monitors the source resource. If the source changes (or if you update the `fromYaml`/`fromString` in the CR), the provider will generate a new commit in the target repository to reflect those changes.

### `override`
*   **`false` (Default - Additive):** The provider will only add new files or update the specific file generated by this CR in the destination repository. It will leave other pre-existing files in the target path untouched.
*   **`true` (Destructive):** The provider will override the existing files in the destination repository's specified path with the files from the source. 

> [!CAUTION]
> Avoid using `override: true` with `path: "/"` as it will overwrite repository service folders like `.git`, `.github`, `.gitignore`, etc.

### Behavior Matrix

| `syncEnabled` | `override` | Behavior |
| :--- | :--- | :--- |
| `false` | `false` | **One-shot Add/Update:** Pushes the file once. Leaves other files in the directory intact. Source fields become immutable. |
| `false` | `true` | **One-shot Replace:** Pushes the file once. **Deletes all other files** in the target `path`. Source fields become immutable. |
| `true` | `false` | **Continuous Sync Add/Update:** Pushes the file and updates it whenever the source changes. Leaves other files in the directory intact. |
| `true` | `true` | **Continuous Sync Replace:** Pushes the file and updates it whenever the source changes. **Deletes all other files** in the target `path` on every sync. |

## Advanced Configuration

The `LocalResource` CR provides several advanced flags to handle complex Git environments.

### Authentication Methods
The `authMethod` field dictates how the provider authenticates with the destination Git server (`toRepo`). Supported values are:
*   **`basic` (Default):** Basic authentication. Requires both `secretRef` (for the token/password) and `usernameRef`.
*   **`bearer`:** Token-based authentication. Requires only `secretRef`. `usernameRef` is ignored.
*   **`cookiefile`:** Authentication via a Git cookie file. Requires `secretRef` (containing the file contents). `usernameRef` is ignored.

### Branch Creation & Orphan Branches
When pushing to the destination repository (`toRepo`), if the target `branch` does not exist, the provider will create it. The behavior is controlled by the `cloneFromBranch` field:
*   **If `cloneFromBranch` is set:** The new branch will be derived from the specified parent branch (e.g., `main`).
*   **If `cloneFromBranch` is omitted:** The provider creates an **orphan branch** (a completely empty branch with no commit history).

### Azure DevOps Compatibility
Azure DevOps requires specific Git transport capabilities (`multi_ack` and `multi_ack_detailed`) that the underlying `go-git` library does not natively implement. 
If the `toRepo.url` contains `dev.azure.com`, you **must** set the following flag to `true` (otherwise the CR validation will fail):
```yaml
spec:
  unsupportedCapabilities: true
```
This forces the library to bypass the capability check, allowing successful communication with Azure.

### Bypassing SSL Verification
If you are interacting with Git servers using self-signed or invalid SSL certificates (e.g., in a local or air-gapped environment), you can disable certificate validation:
```yaml
spec:
  insecure: true
```

## Examples

### Exporting a ConfigMap (fromYaml)
This example pushes a ConfigMap directly to a Git repository.

```yaml
apiVersion: git.krateo.io/v1alpha1
kind: LocalResource
metadata:
  name: export-cm
spec:
  syncEnabled: false
  override: false
  fromResource:
    fromYaml:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: my-app-config
      data:
        logLevel: debug
  toRepo:
    url: https://github.com/your-org/target-repo
    branch: main
    path: /configs
    credentials:
      authMethod: basic
      secretRef:
        name: git-credentials
        namespace: default
        key: token
      usernameRef:
        name: git-credentials
        namespace: default
        key: username
```

### Dynamic Documentation (fromString)
This example dynamically creates a text file using a string and templating, and keeps it synchronized if you update the CR.

```yaml
apiVersion: git.krateo.io/v1alpha1
kind: LocalResource
metadata:
  name: export-string
spec:
  syncEnabled: true
  placeholdersToOverride:
    - name: teamName
      value: platform-ops
  fromResource:
    fileName: team-info.txt
    fromString: |
      This repository is managed by the {{ .teamName }} team.
  toRepo:
    url: https://github.com/your-org/target-repo
    branch: main
    path: /docs
    credentials:
      authMethod: basic
      secretRef:
        name: git-credentials
        namespace: default
        key: token
```