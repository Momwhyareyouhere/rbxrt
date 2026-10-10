package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"rbxrt/internal/engine"
	"rbxrt/internal/hostabi"
)

// Search paths used by dlopen to find the Android platform libraries the guest
// links against. Each directory is scanned by base name.
var searchPaths []string

// SetSearchPaths replaces the directories dlopen searches.
func SetSearchPaths(paths []string) { searchPaths = append([]string(nil), paths...) }

// AddSearchPath appends a directory for dlopen to search.
func AddSearchPath(p string) { searchPaths = append(searchPaths, p) }

// InstallHooks wires the loader's dynamic-linker callbacks into the host library.
// Call this once before running any guest code.
func InstallHooks() {
	hostabi.SetDLHooks(hostabi.DLHooks{
		Open:        dlopen,
		Sym:         dlsym,
		Close:       dlclose,
		Error:       dlerror,
		Addr:        dladdr,
		IteratePhdr: dliteratephdr,
	})
}

var lastErr string

func dlerror() string { return lastErr }

func setErr(format string, args ...any) uintptr {
	lastErr = fmt.Sprintf(format, args...)
	if len(args) == 0 {
		lastErr = format
	}
	return 0
}

// dlopen finds and loads a DSO by base name in the search paths.
func dlopen(name string, flags uintptr) uintptr {
	if name == "" {
		setErr("rbxrt: dlopen: NULL path")
		return 0
	}
	for _, dir := range searchPaths {
		p := filepath.Join(dir, filepath.Base(name))
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			continue
		}
		b, err := engine.Open(p)
		if err != nil {
			setErr("rbxrt: dlopen: %v", err)
			return 0
		}
		im, err := Load(b, Options{})
		if err != nil {
			setErr("rbxrt: dlopen: %v", err)
			return 0
		}
		return im.Base
	}
	// We deliberately do not fabricate a handle: an Android platform library we
	// cannot provide fails exactly as Android would when the library is absent.
	setErr("rbxrt: dlopen: cannot locate %q (searched %d paths)", name, len(searchPaths))
	return 0
}

// dlsym resolves a symbol either against a loaded image or the host library.
func dlsym(handle uintptr, name string) uintptr {
	if name == "" {
		setErr("rbxrt: dlsym: NULL symbol name")
		return 0
	}

	// RTLD_DEFAULT == 0 on Android searches the global namespace.
	if handle == 0 {
		if a, ok := lookupImageSymbol(name); ok {
			return a
		}
		if a, ok := hostabi.Lookup(name); ok {
			return a
		}
		setErr("rbxrt: dlsym: %s not found", name)
		return 0
	}

	// Otherwise handle identifies a specific image (its base address).
	for _, im := range Images() {
		if im.Base == handle {
			if a, ok := im.Exports[name]; ok {
				return a
			}
			setErr("rbxrt: dlsym: %s not found in image", name)
			return 0
		}
	}
	setErr("rbxrt: dlsym: invalid handle %#x", handle)
	return 0
}

// lookupImageSymbol searches every loaded image's exports.
func lookupImageSymbol(name string) (uintptr, bool) {
	for _, im := range Images() {
		if a, ok := im.Exports[name]; ok {
			return a, true
		}
	}
	return 0, false
}

func dlclose(handle uintptr) int {
	for _, im := range Images() {
		if im.Base == handle {
			// Keep the mapping; guest code may still reference it. Just drop it
			// from the namespace so it is no longer visible to dlsym.
			unregisterImage(im)
			return 0
		}
	}
	return -1
}

// dladdr fills struct dl_info for addr.
func dladdr(addr uintptr, info uintptr) int {
	type dlInfo struct {
		Fname uintptr
		Fbase uintptr
		Sname uintptr
		Saddr uintptr
	}
	if info == 0 {
		return 0
	}
	di := (*dlInfo)(unsafe.Pointer(info))
	for _, im := range Images() {
		if addr >= im.Base && addr < im.End {
			di.Fbase = im.Base
			// dli_fname is a path the guest may print; use a NUL-terminated copy.
			di.Fname = hostabi.CString(im.Eng.Path)
			for n, a := range im.Exports {
				if a == addr {
					di.Sname = hostabi.CString(n)
					di.Saddr = a
					break
				}
			}
			return 1
		}
	}
	return 0
}

// dliteratephdr invokes the guest callback once per loaded image.
//
// struct dl_phdr_info (x86-64, Android; bionic adds four trailing fields):
//
//	ElfW(Addr) dlpi_addr;         // +0  load bias
//	const char* dlpi_name;        // +8
//	const ElfW(Phdr)* dlpi_phdr;  // +16
//	ElfW(Half) dlpi_phnum;        // +24
//	unsigned long long dlpi_adds; // +32
//	unsigned long long dlpi_subs; // +40
//	size_t dlpi_tls_modid;        // +48
//	void* dlpi_tls_data;          // +56
func dliteratephdr(cb uintptr, data uintptr) int {
	var info [64]byte
	for _, im := range Images() {
		phdrs := im.Eng.File.Progs
		*(*uintptr)(unsafe.Pointer(&info[0])) = uintptr(im.Bias)
		*(*uintptr)(unsafe.Pointer(&info[8])) = hostabi.CString(im.Eng.Path)
		*(*uintptr)(unsafe.Pointer(&info[16])) = uintptr(unsafe.Pointer(&phdrs[0]))
		*(*uint16)(unsafe.Pointer(&info[24])) = uint16(len(phdrs))
		info[28] = 0 // padding
		if hostabi.CallGuestInt3(cb, uintptr(unsafe.Pointer(&info[0])), 64, data) != 0 {
			return 0
		}
	}
	return 0
}
