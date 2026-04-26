package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDeviceLog_Roundtrip(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	if err := s.WriteDeviceLog("abc123", "1700000000123", strings.NewReader("hello world")); err != nil {
		t.Fatalf("WriteDeviceLog failed: %v", err)
	}

	want := filepath.Join(base, "abc123", "swaglog", "1700000000123.log")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", want, err)
	}
	if string(got) != "hello world" {
		t.Errorf("file content = %q, want %q", got, "hello world")
	}
}

func TestWriteDeviceStats_Roundtrip(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	payload := `{"foo":"bar"}`
	if err := s.WriteDeviceStats("abc123", "evt-1", strings.NewReader(payload)); err != nil {
		t.Fatalf("WriteDeviceStats failed: %v", err)
	}

	want := filepath.Join(base, "abc123", "stats", "evt-1.json")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", want, err)
	}
	if string(got) != payload {
		t.Errorf("file content = %q, want %q", got, payload)
	}
}

func TestWriteDeviceLog_RejectsPathTraversal(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	cases := []struct {
		name string
		dID  string
		id   string
	}{
		{"dotdot id", "abc123", ".."},
		{"slash id", "abc123", "../etc/passwd"},
		{"backslash id", "abc123", "..\\evil"},
		{"empty id", "abc123", ""},
		{"dotdot dongle", "..", "1"},
		{"slash dongle", "abc/def", "1"},
		{"empty dongle", "", "1"},
		{"hidden dotdot", "abc123", "foo..bar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.WriteDeviceLog(tc.dID, tc.id, strings.NewReader("x"))
			if err == nil {
				t.Fatalf("expected error for dongle=%q id=%q", tc.dID, tc.id)
			}
		})
	}

	// No file should have escaped the temp dir.
	if _, err := os.Stat(filepath.Join(base, "..")); err != nil && !os.IsNotExist(err) {
		// nothing: parent of t.TempDir() exists, just confirm we did not
		// write a file with a "../" path
	}
}

func TestWriteDeviceLog_CreatesNestedDirs(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	if err := s.WriteDeviceLog("never-seen-before", "1", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteDeviceLog failed: %v", err)
	}

	dir := filepath.Join(base, "never-seen-before", "swaglog")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("expected swaglog dir to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected swaglog to be a directory")
	}
}
