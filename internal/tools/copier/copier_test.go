package copier

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/krateoplatformops/git-provider/internal/tools/template"
)

func writeFile(t *testing.T, fsFileSystem billy.Filesystem, path string, data string) {
	t.Helper()
	dir := filepath.Dir(path)
	if dir != "." {
		if err := fsFileSystem.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdirall %s: %v", dir, err)
		}
	}
	f, err := fsFileSystem.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, fsFileSystem billy.Filesystem, path string) string {
	t.Helper()
	f, err := fsFileSystem.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestRenderFileNamesAndContent(t *testing.T) {
	from := memfs.New()
	to := memfs.New()

	// source file with templated name and content
	writeFile(t, from, "/src/file_{%.name%}.txt", "hello {%.name%}")

	co, err := NewCopier(from, to, WithOriginCopyPath("/src"), WithTargetCopyPath("/dst"), WithIgnorePath("/"), WithGoTemplate([]template.TemplateValue{{Key: "name", Value: "world"}}))
	if err != nil {
		t.Fatalf("failed to create copier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	got := readFile(t, to, "/dst/file_world.txt")
	if got != "hello world" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestMustacheRenderFileNamesAndContent(t *testing.T) {
	from := memfs.New()
	to := memfs.New()

	// source file with templated name and content
	writeFile(t, from, "/src/file_{{name}}.txt", "hello {{name}}")

	co, err := NewCopier(from, to, WithOriginCopyPath("/src"), WithTargetCopyPath("/dst"), WithIgnorePath("/"), WithMustacheTemplate(map[string]string{"name": "world"}))
	if err != nil {
		t.Fatalf("failed to create copier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	got := readFile(t, to, "/dst/file_world.txt")
	if got != "hello world" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestMustacheRendering(t *testing.T) {
	from := memfs.New()
	to := memfs.New()

	// source file with templated content
	writeFile(t, from, "/src/greet.txt", "Hello {{name}}!")

	co, err := NewCopier(from, to, WithOriginCopyPath("/src"), WithTargetCopyPath("/dst"), WithIgnorePath("/"), WithMustacheTemplate(map[string]string{"name": "Krateo"}))
	if err != nil {
		t.Fatalf("failed to create copier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	got := readFile(t, to, "/dst/greet.txt")
	if got != "Hello Krateo!" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestKrateoIgnorePreventsRendering(t *testing.T) {
	from := memfs.New()
	to := memfs.New()

	// create ignore file that matches the source file path
	writeFile(t, from, "/.krateoignore", "/src/ignored.txt\n")

	// source file contains a template but is listed in .krateoignore -> should NOT be rendered
	writeFile(t, from, "/src/ignored.txt", "value: {{name}}")

	co, err := NewCopier(from, to, WithOriginCopyPath("/src"), WithTargetCopyPath("/dst"), WithIgnorePath("/"), WithGoTemplate([]template.TemplateValue{{Key: "name", Value: "X"}}))
	if err != nil {
		t.Fatalf("failed to create copier: %v", err)
	}
	if err := co.Copy(true); err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	got := readFile(t, to, "/dst/ignored.txt")
	if got != "value: {{name}}" {
		t.Fatalf("file was rendered despite being ignored: %q", got)
	}
}

func TestTargetIgnoreSkipsExisting(t *testing.T) {
	from := memfs.New()
	to := memfs.New()

	// source has two files
	writeFile(t, from, "/src/skip.txt", "from-skip")
	writeFile(t, from, "/src/keep.txt", "from-keep")

	// create an existing file in target (TO FS) that should be considered for ignoring
	writeFile(t, to, "/dst/skip.txt", "to-skip-original")

	co, err := NewCopier(from, to, WithOriginCopyPath("/src"), WithTargetCopyPath("/dst"), WithIgnorePath("/"))
	if err != nil {
		t.Fatalf("failed to create copier: %v", err)
	}
	// override == false so setTargetIgnore is invoked and skip.txt should be ignored (not overwritten)
	if err := co.Copy(false); err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	// skip.txt should remain the original in target (not overwritten)
	gotSkip := readFile(t, to, "/dst/skip.txt")
	if gotSkip != "to-skip-original" {
		t.Fatalf("skip.txt was overwritten or missing: %q", gotSkip)
	}

	// keep.txt should be copied from source
	gotKeep := readFile(t, to, "/dst/keep.txt")
	if gotKeep != "from-keep" {
		t.Fatalf("keep.txt not copied correctly: %q", gotKeep)
	}
}
