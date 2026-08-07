package architecture_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestProductionDependencyDirection(t *testing.T) {
	t.Parallel()

	allowed := map[string]map[string]bool{
		"raft":          {},
		"storage":       {"raft": true, "statemachine": true},
		"transport":     {"raft": true},
		"statemachine":  {},
		"service":       {"observability": true, "raft": true, "security": true, "statemachine": true},
		"security":      {},
		"observability": {},
		"checker":       {},
		"chaos":         {"raft": true, "storage": true, "testkit": true, "transport": true},
		"testkit":       {},
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate architecture test")
	}
	internalRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
	modulePrefix := readModulePath(t, filepath.Join(internalRoot, "..")) + "/internal/"

	entries, err := os.ReadDir(internalRoot)
	if err != nil {
		t.Fatalf("list internal modules: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if isProductionGoFile(entry.Name()) {
				t.Errorf("production Go file %s must belong to a classified internal module", entry.Name())
			}
			continue
		}
		if entry.Name() == "architecture" {
			rejectProductionGoFiles(t, filepath.Join(internalRoot, entry.Name()))
			continue
		}
		if _, classified := allowed[entry.Name()]; !classified {
			t.Errorf("internal module %s has no dependency policy", entry.Name())
		}
	}

	for source, permitted := range allowed {
		sourcePath := filepath.Join(internalRoot, source)
		err := filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			return checkImports(t, modulePrefix, source, permitted, path)
		})
		if err != nil {
			t.Fatalf("scan %s: %v", source, err)
		}
	}
}

func rejectProductionGoFiles(t *testing.T, root string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && isProductionGoFile(entry.Name()) {
			t.Errorf("production Go file %s bypasses internal dependency policy", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan architecture package: %v", err)
	}
}

func isProductionGoFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

func readModulePath(t *testing.T, root string) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	fields := strings.Fields(string(contents))
	if len(fields) < 2 || fields[0] != "module" {
		t.Fatal("go.mod must begin with a module directive")
	}
	return fields[1]
}

func checkImports(t *testing.T, modulePrefix, source string, permitted map[string]bool, path string) error {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return err
	}
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(importPath, modulePrefix) {
			continue
		}
		target := strings.Split(strings.TrimPrefix(importPath, modulePrefix), "/")[0]
		if target != source && !permitted[target] {
			t.Errorf("%s imports forbidden internal module %s in %s", source, target, path)
		}
	}
	return nil
}
