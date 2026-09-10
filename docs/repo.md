# Repo

The `Repo` Custom Resource (CR) manages Git-to-Git operations. It clones a source repository, optionally applies templates to its contents, and pushes the result to a destination repository.

## Overview

`Repo` is designed for scenarios where you need to copy an entire repository (or a specific path within it) to another location. 

**Why use it?** 
A common use case is **project bootstrapping**. You might have a "golden path" or "skeleton" repository containing standard boilerplate code, CI/CD pipelines, and configuration files. When a developer requests a new project, you can use the `Repo` CR to clone this skeleton, inject project-specific values (like project name, ports, or namespaces) via templating, and push the customized result to a brand new Git repository for the developer to use.

## Templating

> **Delimiters.** Placeholders use `{% %}`, not `{{ }}`, so they do not collide with Helm syntax in the files being copied. Override with `krateo.io/templating-delims: "left,right"`. See [local-resource.md](local-resource.md#delimiters) for the reasoning.
>
> **Failures.** `status.templatingErrors` lists every file that could not be rendered, with the renderer's message. A failed sync commits nothing.


The `git-provider` supports two templating engines to customize files copied from the source repository: **Mustache** (default) and **Go Templates**.

### Providing Values
Templating values are provided via a referenced `ConfigMap` defined in `spec.configMapKeyRef`. The referenced key in the `ConfigMap` must contain a valid JSON string representing the key-value pairs for the templates.

```yaml
spec:
  configMapKeyRef:
    name: my-template-values
    namespace: default
    key: values # This key in the ConfigMap contains the JSON string
```

### Mustache (Default)
By default, the provider uses the [Mustache](https://mustache.github.io) templating engine with `{{ }}` delimiters. 

> [!TIP]
> If you are templating files where `{{ }}` conflicts with the file's native syntax (e.g., Helm charts or GitHub Actions), you can specify custom delimiters in the **first line** of the file you want to template.
> 
> For example, to change the delimiters to `<% %>`, add this as the first line:
> ```text
> {{=<% %>=}}
> ```
> Then use `<% myValue %>` in the rest of the file. [See custom delimiter reference](https://github.com/janl/mustache.js/?tab=readme-ov-file#setting-in-templates).

### Go Templates
If you prefer [Go templates](https://pkg.go.dev/text/template), you can enable it by adding a specific annotation to your `Repo` CR:

```yaml
apiVersion: git.krateo.io/v1alpha1
kind: Repo
metadata:
  name: test-repo
  annotations:
    krateo.io/templating-engine: "gotemplate"
```

> [!NOTE]
> The Go template engine includes the [Sprig function library](http://masterminds.github.io/sprig/), providing a wide variety of template functions (e.g., string manipulation, math, default values).

### File Name Templating
If you need to template the filename itself, you can only use the default `{{ }}` delimiters in the filename string (e.g., `{{ your-prop }}.yaml`), regardless of the engine chosen.

## Synchronization & Overrides

The behavior of how the `Repo` CR interacts with the destination repository over time is controlled by two key flags: `enableUpdate` and `override`.

### `enableUpdate`
*   **`false` (Default):** The provider performs a "one-shot" execution. It clones the source, templates it, and pushes it to the destination once. Subsequent commits to the source repository will **not** trigger a new push to the destination.
*   **`true`:** The provider continuously monitors the source repository. When newer commits are retrieved from the `fromRepo`, the provider performs updates on the repository specified in `toRepo`, re-applying templates and pushing the changes.

### `override`
*   **`false` (Default - Additive):** The provider will only add new files and update existing templated files in the destination repository. It will leave other pre-existing files in the destination path untouched.
*   **`true` (Destructive):** The provider will override the existing files in the destination repository with the files from the source repository. 

> [!WARNING]  
> Avoid using `override: true` if both `fromRepo.path` and `toRepo.path` are `/` (the root). This will override and potentially delete service folders like `.git`, `.github`, `.gitignore`, etc., in the destination repository!

### Behavior Matrix

| `enableUpdate` | `override` | Behavior |
| :--- | :--- | :--- |
| `false` | `false` | **One-shot Add/Update:** Copies and templates files once. Leaves other files in the destination directory intact. |
| `false` | `true` | **One-shot Replace:** Copies and templates files once. **Deletes all other files** in the destination `path`. |
| `true` | `false` | **Continuous Sync Add/Update:** Re-syncs whenever the source repo gets new commits. Leaves other files in the destination directory intact. |
| `true` | `true` | **Continuous Sync Replace:** Re-syncs whenever the source repo gets new commits. **Deletes all other files** in the destination `path` on every sync. |

## Advanced Configuration

The `Repo` CR provides several advanced flags to handle complex Git environments.

### Authentication Methods
The `authMethod` field dictates how the provider authenticates with the remote Git servers (`fromRepo` and `toRepo`). Supported values are:
*   **`generic` (Default):** Basic authentication. Requires both `secretRef` (for the token/password) and `usernameRef`.
*   **`bearer`:** Token-based authentication. Requires only `secretRef`. `usernameRef` is ignored.
*   **`cookiefile`:** Authentication via a Git cookie file. Requires `secretRef` (containing the file contents). `usernameRef` is ignored.

### Branch Creation & Orphan Branches
When pushing to the destination repository (`toRepo`), if the target `branch` does not exist, the provider will create it. The behavior is controlled by the `cloneFromBranch` field:
*   **If `cloneFromBranch` is set:** The new branch will be derived from the specified parent branch (e.g., `main`).
*   **If `cloneFromBranch` is omitted:** The provider creates an **orphan branch** (a completely empty branch with no commit history).

### Azure DevOps Compatibility
Azure DevOps requires specific Git transport capabilities (`multi_ack` and `multi_ack_detailed`) that the underlying `go-git` library does not natively implement. 
To clone or push to Azure DevOps, you **must** set the following flag to `true`:
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

### Excluding Files (.krateoignore)
You can prevent specific files or directories from being copied from the source repository by defining a "krateo ignore" file (which uses the exact same syntax as `.gitignore`).
Specify the path to this file within the source repository using `krateoIgnorePath`. If not set, it defaults to looking for a file at `/` (the root).
```yaml
spec:
  fromRepo:
    krateoIgnorePath: .krateoignore
```

## Examples

### Bootstrapping a Project with Go Templates

#### 1. ConfigMap Manifest
Create a ConfigMap containing the values for your templates:
```yaml 
apiVersion: v1
kind: ConfigMap
metadata:
  name: project-values
data:
  values: |
    { 
      "projectName": "my-awesome-api",
      "servicePort": "8080"
    }
```

#### 2. Repo Manifest
Create the `Repo` CR to initiate the project bootstrap:
```yaml
apiVersion: git.krateo.io/v1alpha1
kind: Repo
metadata:
  name: bootstrap-api
  annotations:
    krateo.io/templating-engine: "gotemplate"
spec:
  enableUpdate: false # We only want to bootstrap it once
  override: false
  configMapKeyRef:
    key: values
    name: project-values
    namespace: default
  fromRepo:
    authMethod: generic
    branch: main
    path: templates/go-api-skeleton
    url: https://github.com/my-org/golden-paths
    # ... secretRefs omitted for brevity ...
  toRepo:
    authMethod: generic
    branch: main
    path: /
    url: https://github.com/my-org/my-awesome-api
    # ... secretRefs omitted for brevity ...
```