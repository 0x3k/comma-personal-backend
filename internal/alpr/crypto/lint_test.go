package crypto

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLint_NoAESNewCipherOutsidePackage enforces the convention that ALL
// plate-related encryption goes through this package. It scans every .go
// production file under the repository (excluding _test.go and this
// package itself) and fails if it sees aes.NewCipher anywhere else.
//
// The check is a pragmatic guardrail, not a security boundary: a determined
// edit could route around it by importing aes via an alias. The point is
// to make accidental drift LOUD so a reviewer sees it.
//
// If a legitimate non-ALPR aes.NewCipher use case ever appears, narrow this
// test to the ALPR subtree (internal/alpr/, internal/api/alpr/, ...) rather
// than disabling it.
func TestLint_NoAESNewCipherOutsidePackage(t *testing.T) {
	root := repoRoot(t)
	const needle = "aes.NewCipher"

	// Directories whose contents are out of scope:
	// - this package itself (the only legitimate caller)
	// - vendor (third-party code, not ours)
	// - node_modules / .git / web (frontend assets, unrelated)
	skipDirs := map[string]bool{
		".git":         true,
		"node_modules": true,
		"web":          true,
		"vendor":       true,
		".claude":      true, // local agent worktrees and hooks
		".projd":       true, // workflow scripts
	}

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip files inside this package -- they are the legitimate home.
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, filepath.Join("internal", "alpr", "crypto")+string(filepath.Separator)) ||
			rel == filepath.Join("internal", "alpr", "crypto") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), needle) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("aes.NewCipher must only be referenced from internal/alpr/crypto.\n"+
			"Found in: %s\n"+
			"If you have a legitimate non-ALPR AES use case, narrow the lint scope rather than disabling it.",
			strings.Join(offenders, ", "))
	}
}

// repoRoot walks upward from the test's working directory until it finds
// a go.mod file. The crypto package sits three levels deep
// (internal/alpr/crypto), so the repo root is reachable in a few steps.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 16; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
	t.Fatalf("repo root search exceeded depth limit")
	return ""
}
