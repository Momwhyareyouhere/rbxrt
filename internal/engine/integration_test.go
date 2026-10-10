package engine

import (
	"os"
	"testing"
)

// TestLibrobloxGolden pins the engine's analysis of the genuine Roblox
// x86_64 library against recorded values. It runs only when the library is
// present on the developer machine (it is not part of the repo); CI without
// the artifact skips it.
func TestLibrobloxGolden(t *testing.T) {
	const path = "/home/bombo/extracted/lib/x86_64/libroblox.so"
	if !fileExists(path) {
		t.Skip("genuine libroblox.so not present")
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if b.SOName != "libroblox.so" {
		t.Errorf("soname = %q, want libroblox.so", b.SOName)
	}

	b.ClassifyImports()
	if got := len(b.Imports); got != 576 {
		t.Errorf("imports = %d, want 576", got)
	}
	if got := len(b.Exports); got != 561 {
		t.Errorf("exports = %d, want 561", got)
	}

	r := b.ReportClassification()
	if got := r.ByClass[ClassHost]; got != 419 {
		t.Errorf("host class count = %d, want 419", got)
	}
	if got := r.ByClass[ClassAndroidLib]; got != 151 {
		t.Errorf("android-lib class count = %d, want 151", got)
	}
	if got := r.ByClass[ClassStub]; got != 6 {
		t.Errorf("stub class count = %d, want 6", got)
	}
	for _, s := range r.Stubs {
		if s.Bind != "WEAK" {
			t.Errorf("stub %q is %s, want weak (binds to zero)", s.Name, s.Bind)
		}
	}
}

// TestLibrobloxGoldenRelocs pins the packed-relocation inventory of the
// genuine library.
func TestLibrobloxGoldenRelocs(t *testing.T) {
	const path = "/home/bombo/extracted/lib/x86_64/libroblox.so"
	if !fileExists(path) {
		t.Skip("genuine libroblox.so not present")
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if len(b.Relocs.Relative) != 572132 {
		t.Errorf("relative relocs = %d, want 572132", len(b.Relocs.Relative))
	}
	if len(b.Relocs.Other) != 627 {
		t.Errorf("other relocs = %d, want 627", len(b.Relocs.Other))
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
