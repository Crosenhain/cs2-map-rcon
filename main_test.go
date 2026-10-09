package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTargetsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rcon_targets")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRconTargetsFromFile(t *testing.T) {
	path := writeTargetsFile(t, strings.Join([]string{
		"# comment",
		"",
		" Main = 192.168.1.10:27015 = pass=word = 123 ",
		"broken line",
		"Retakes=192.168.1.10:27016=adminpass=",
	}, "\n"))

	got := loadRconTargetsFromFile(path)
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	// SplitN(…, 4) keeps any extra "=" in the collection ID, not the password.
	if m := got["Main"]; m.Address != "192.168.1.10:27015" || m.Password != "pass" || m.CollectionID != "word = 123" {
		t.Errorf("Main parsed as %+v", m)
	}
	if r := got["Retakes"]; r.Password != "adminpass" || r.CollectionID != "" {
		t.Errorf("Retakes parsed as %+v", r)
	}
}

func TestLoadRconTargetsFromFileErrors(t *testing.T) {
	if got := loadRconTargetsFromFile(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Errorf("missing file: got %+v, want no targets", got)
	}

	// A line longer than bufio.Scanner's 64 KiB token limit makes Scan stop
	// early with an error; the valid line before it must not be used alone.
	path := writeTargetsFile(t, "Main=127.0.0.1:27015=pw=1\n"+strings.Repeat("x", 70*1024)+"\n")
	if got := loadRconTargetsFromFile(path); len(got) != 0 {
		t.Errorf("partially read file: got %+v, want no targets", got)
	}
}
