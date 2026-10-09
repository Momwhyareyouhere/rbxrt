// Package apk models a Roblox Android App Bundle installation (a base APK plus
// any split APKs) and reads the pieces the runtime needs: the binary manifest,
// the native libraries, and the cryptographic signatures.
//
// Nothing in this package trusts the APK. Verification lives in signing.go, and
// callers are expected to gate every other operation on a successful Verify.
package apk

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Archive is one APK file on disk, open for random access.
type Archive struct {
	Path string
	Size int64
	zr   *zip.Reader
	cl   io.Closer
}

// OpenArchive opens a single APK.
func OpenArchive(p string) (*Archive, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("apk: %s: not a zip/APK: %w", p, err)
	}
	return &Archive{Path: p, Size: fi.Size(), zr: zr, cl: f}, nil
}

// Close releases the underlying file.
func (a *Archive) Close() error { return a.cl.Close() }

// Entries returns the archive's file names, sorted.
func (a *Archive) Entries() []string {
	out := make([]string, 0, len(a.zr.File))
	for _, f := range a.zr.File {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether the archive contains the named entry.
func (a *Archive) Has(name string) bool { return a.find(name) != nil }

func (a *Archive) find(name string) *zip.File {
	for _, f := range a.zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Open opens a single entry for reading.
func (a *Archive) Open(name string) (io.ReadCloser, error) {
	f := a.find(name)
	if f == nil {
		return nil, fmt.Errorf("apk: %s: no entry %q", a.Path, name)
	}
	return f.Open()
}

// Read returns the full contents of an entry.
func (a *Archive) Read(name string) ([]byte, error) {
	f := a.find(name)
	if f == nil {
		return nil, fmt.Errorf("apk: %s: no entry %q", a.Path, name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Extract writes the named entry to dst (a file path).
func (a *Archive) Extract(name, dst string) error {
	rc, err := a.Open(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Set is a base APK plus its split APKs, all for the same package.
type Set struct {
	Archives []*Archive
}

// OpenSet opens every path and detects the base APK. The base is the archive
// that carries AndroidManifest.xml (or, failing that, the one named base.apk).
func OpenSet(paths []string) (*Set, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("apk: no APKs given")
	}
	s := &Set{}
	for _, p := range paths {
		a, err := OpenArchive(p)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.Archives = append(s.Archives, a)
	}
	if s.Base() == nil {
		s.Close()
		return nil, fmt.Errorf("apk: no base APK among %v (none has AndroidManifest.xml)", paths)
	}
	return s, nil
}

// Base returns the base APK, or nil if none in the set carries a manifest.
func (s *Set) Base() *Archive {
	var fallback *Archive
	for _, a := range s.Archives {
		if a.Has("AndroidManifest.xml") {
			return a
		}
		if strings.EqualFold(baseName(a.Path), "base.apk") && fallback == nil {
			fallback = a
		}
	}
	return fallback
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Splits returns the non-base archives.
func (s *Set) Splits() []*Archive {
	base := s.Base()
	var out []*Archive
	for _, a := range s.Archives {
		if a != base {
			out = append(out, a)
		}
	}
	return out
}

// Close releases every file. It is safe to call on a partially built set.
func (s *Set) Close() {
	for _, a := range s.Archives {
		if a != nil && a.cl != nil {
			_ = a.cl.Close()
		}
	}
}

// NativeLib is one shared library found in the APK set.
type NativeLib struct {
	Name    string // e.g. "libroblox.so"
	ABI     string // e.g. "x86_64"
	Archive *Archive
	Entry   string // e.g. "lib/x86_64/libroblox.so"
}

// NativeLibs lists every lib/<abi>/*.so across the base and split APKs.
func (s *Set) NativeLibs() []NativeLib {
	var out []NativeLib
	for _, a := range s.Archives {
		for _, name := range a.Entries() {
			if !strings.HasPrefix(name, "lib/") || !strings.HasSuffix(name, ".so") {
				continue
			}
			rest := strings.TrimPrefix(name, "lib/")
			i := strings.IndexByte(rest, '/')
			if i < 0 {
				continue
			}
			out = append(out, NativeLib{
				Name:    rest[i+1:],
				ABI:     rest[:i],
				Archive: a,
				Entry:   name,
			})
		}
	}
	return out
}

// FindLib returns the first native library with the given base name (e.g.
// "libroblox.so"), preferring the requested ABI when given.
func (s *Set) FindLib(name, abi string) (NativeLib, bool) {
	libs := s.NativeLibs()
	if abi != "" {
		for _, l := range libs {
			if l.Name == name && l.ABI == abi {
				return l, true
			}
		}
	}
	for _, l := range libs {
		if l.Name == name {
			return l, true
		}
	}
	return NativeLib{}, false
}

// ABIs returns the distinct ABIs present across the set, sorted.
func (s *Set) ABIs() []string {
	seen := map[string]bool{}
	for _, l := range s.NativeLibs() {
		seen[l.ABI] = true
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
