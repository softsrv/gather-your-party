package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoGoogleCalendarAPIUsage(t *testing.T) {
	root := "."
	fset := token.NewFileSet()

	skipDirs := map[string]bool{
		"vendor":       true,
		"node_modules": true,
		".git":         true,
	}

	forbiddenSubstring := "googleapis.com/calendar/"
	forbiddenImportPrefix := "google.golang.org/api/calendar"

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			name := d.Name()
			if skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}

		// Only check Go source files that are not test files
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		// Read file content and check for forbidden endpoint substrings
		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("failed to read %s: %v", path, err)
			return nil
		}

		if strings.Contains(string(content), forbiddenSubstring) {
			t.Errorf("%s contains forbidden calendar API path %q", path, forbiddenSubstring)
		}

		// Parse AST and check imports for forbidden client packages
		node, err := parser.ParseFile(fset, path, content, parser.ImportsOnly)
		if err != nil {
			t.Errorf("failed to parse imports of %s: %v", path, err)
			return nil
		}

		for _, imp := range node.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == forbiddenImportPrefix || strings.HasPrefix(importPath, forbiddenImportPrefix+"/") {
				t.Errorf("%s imports forbidden Google Calendar package %q", path, importPath)
			}
		}

		return nil
	})

	if err != nil {
		t.Fatalf("filepath.WalkDir error: %v", err)
	}
}
