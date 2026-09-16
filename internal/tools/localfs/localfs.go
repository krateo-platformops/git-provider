package localfs

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/krateo-platformops/plumbing/jqutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/yaml"
)

type LocalFS struct {
	billy.Filesystem
	tmp string
}

func NewLocalFS(path string) (*LocalFS, error) {
	tmpDir, err := os.MkdirTemp(path, "git-provider-local-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary directory: %w", err)
	}

	diskFS := osfs.New(tmpDir)

	return &LocalFS{Filesystem: diskFS, tmp: tmpDir}, nil
}

func (lfs LocalFS) Cleanup() error {
	if err := os.RemoveAll(lfs.tmp); err != nil {
		return fmt.Errorf("failed to remove temporary directory: %w", err)
	}
	return nil
}

func (lfs LocalFS) WriteStringResource(filename string, content string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("filename must be provided when writing fromString resource")
	}
	// Write the fromResource content to the temporary local filesystem
	fi, err := lfs.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating file in local filesystem: %w", err)
	}
	defer fi.Close()

	_, err = fi.Write([]byte(content))
	if err != nil {
		return "", fmt.Errorf("writing string data to file: %w", err)
	}
	return filename, nil
}

// WriteK8sResource writes a K8s manifest to the local filesystem, ensuring it has the required fields.
// If filename is empty, it generates a filename based on the resource's GVK and name.
// It returns the filename used.
func (lfs LocalFS) WriteK8sResource(filename string, manifest runtime.RawExtension) (string, error) {
	var obj runtime.Object
	var scope conversion.Scope
	err := runtime.Convert_runtime_RawExtension_To_runtime_Object(&manifest, &obj, scope)
	if err != nil {
		return "", fmt.Errorf("converting fromResource.FromYaml to runtime.Object: %w", err)
	}
	res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return "", fmt.Errorf("converting fromResource.FromYaml to unstructured: %w", err)
	}
	ures := &unstructured.Unstructured{Object: res}

	// checks that the required fields are present. Kind, APIVersion and (Name or GenerateName) are required
	if ures.GetKind() == "" || ures.GetAPIVersion() == "" || (ures.GetName() == "" && ures.GetGenerateName() == "") {
		return "", fmt.Errorf("resource is missing required fields (kind, apiVersion, name or generateName)")
	}
	stringGVK := fmt.Sprintf("%s.%s.%s",
		ures.GroupVersionKind().Kind,
		ures.GroupVersionKind().Version,
		ures.GroupVersionKind().Group,
	)
	if filename == "" {
		if ures.GetNamespace() == "" {
			filename = fmt.Sprintf("%s_%s.yaml",
				stringGVK,
				ures.GetName(),
			)
		} else {
			filename = fmt.Sprintf("%s_%s_%s.yaml",
				stringGVK,
				ures.GetName(),
				ures.GetNamespace(),
			)
		}
	}

	// Write the fromResource content to the temporary local filesystem
	fi, err := lfs.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating file in local filesystem: %w", err)
	}
	defer fi.Close()

	scheme := runtime.NewScheme()
	serializer := json.NewYAMLSerializer(
		json.DefaultMetaFactory,
		scheme,
		scheme,
	)

	var buf bytes.Buffer
	err = serializer.Encode(obj, &buf)
	if err != nil {
		return "", fmt.Errorf("encoding object: %w", err)
	}

	manifestString := buf.String()

	_, err = fi.Write([]byte(manifestString))
	if err != nil {
		return "", fmt.Errorf("writing YAML data to file: %w", err)
	}
	return filename, nil
}

func (lfs LocalFS) WriteK8sResourceJQ(filename string, manifest runtime.RawExtension, jqFilters ...string) (string, error) {
	var obj runtime.Object
	var scope conversion.Scope
	err := runtime.Convert_runtime_RawExtension_To_runtime_Object(&manifest, &obj, scope)
	if err != nil {
		return "", fmt.Errorf("converting fromResource.FromYaml to runtime.Object: %w", err)
	}

	res, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return "", fmt.Errorf("converting fromResource.FromYaml to unstructured: %w", err)
	}
	ures := &unstructured.Unstructured{Object: res}

	// Validate required fields
	if ures.GetKind() == "" || ures.GetAPIVersion() == "" || (ures.GetName() == "" && ures.GetGenerateName() == "") {
		return "", fmt.Errorf("resource is missing required fields (kind, apiVersion, name or generateName)")
	}

	// Generate filename if necessary
	stringGVK := fmt.Sprintf("%s.%s.%s",
		ures.GroupVersionKind().Kind,
		ures.GroupVersionKind().Version,
		ures.GroupVersionKind().Group,
	)
	if filename == "" {
		if ures.GetNamespace() == "" {
			filename = fmt.Sprintf("%s_%s.yaml", stringGVK, ures.GetName())
		} else {
			filename = fmt.Sprintf("%s_%s_%s.yaml", stringGVK, ures.GetName(), ures.GetNamespace())
		}
	}
	var processedJSON string
	if len(jqFilters) > 0 {
		// Apply jq filters sequentially
		for _, filter := range jqFilters {
			processedJSON, err = jqutil.Eval(context.Background(), jqutil.EvalOptions{
				Query:        filter,
				Data:         ures.Object,
				Unquote:      false,
				ModuleLoader: nil,
			})
			if err != nil {
				return "", fmt.Errorf("applying jq filter '%s': %w", filter, err)
			}
			// Update ures.Object for the next iteration
			ures.Object = make(map[string]any)
			err = yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(processedJSON)), 1024).Decode(&ures.Object)
			if err != nil {
				return "", fmt.Errorf("decoding JSON after jq filter '%s': %w", filter, err)
			}
		}
	} else {
		processedJSON = string(manifest.Raw)
	}

	scheme := runtime.NewScheme()
	serializer := json.NewYAMLSerializer(
		json.DefaultMetaFactory,
		scheme,
		scheme,
	)

	// serialize the processed JSON back into a runtime.Object
	objProcessed := &unstructured.Unstructured{}
	err = runtime.DecodeInto(unstructured.UnstructuredJSONScheme, []byte(processedJSON), objProcessed)
	if err != nil {
		return "", fmt.Errorf("decoding processed JSON into runtime.Object: %w", err)
	}

	var buf bytes.Buffer
	err = serializer.Encode(objProcessed, &buf)
	if err != nil {
		return "", fmt.Errorf("encoding object: %w", err)
	}
	// Write the processed content to the temporary local filesystem
	fi, err := lfs.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("creating file in local filesystem: %w", err)
	}
	defer fi.Close()

	_, err = fi.Write(buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("writing YAML data to file: %w", err)
	}

	return filename, nil
}
