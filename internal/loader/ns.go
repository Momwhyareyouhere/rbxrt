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

// debugDL logs every guest dlopen/dlsym and its result (RBXRT_DEBUG_DL).
var debugDL = os.Getenv("RBXRT_DEBUG_DL") != ""

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
	h := dlopenImpl(name, flags)
	if debugDL {
		fmt.Fprintf(os.Stderr, "rbxrt: dl: dlopen(%q) -> %#x\n", name, h)
	}
	return h
}

func dlopenImpl(name string, flags uintptr) uintptr {
	if name == "" {
		setErr("rbxrt: dlopen: NULL path")
		return 0
	}
	if h := hostabi.VirtualLibrary(name); h != 0 {
		return h // provided by the runtime itself (AAudio, NDK media, ...)
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
	a := dlsymImpl(handle, name)
	if debugDL {
		fmt.Fprintf(os.Stderr, "rbxrt: dl: dlsym(%#x, %q) -> %#x\n", handle, name, a)
	}
	return a
}

func dlsymImpl(handle uintptr, name string) uintptr {
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

	if hostabi.IsVirtualLibrary(handle) {
		if a, ok := hostabi.VirtualSymbol(name); ok {
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
		// im.Eng.File is a *debug/elf.File whose Progs field is []*elf.Prog, a
		// slice of Go pointers. The guest callback (glibc/libgcc's
		// _Unwind_Find_FDE) expects dlpi_phdr to point at a *contiguous array of
		// raw Elf64_Phdr* — the file's program-header table. Handing it the Go
		// pointer slice made it read pointers as Phdr fields, so no module ever
		// matched, EH frames were never found, and every C++ exception unwound
		// to std::terminate. Serialize the headers explicitly (endian-correct,
		// independent of Go struct layout) into one flat byte slice.
		const phdrSize = 56 // sizeof(Elf64_Phdr)
		progs := im.Eng.File.Progs
		raw := make([]byte, phdrSize*len(progs))
		bo := im.Eng.File.ByteOrder
		for i, p := range progs {
			o := i * phdrSize
			bo.PutUint32(raw[o+0:], uint32(p.Type))
			bo.PutUint32(raw[o+4:], uint32(p.Flags))
			bo.PutUint64(raw[o+8:], p.Off)
			bo.PutUint64(raw[o+16:], p.Vaddr)
			bo.PutUint64(raw[o+24:], p.Paddr)
			bo.PutUint64(raw[o+32:], p.Filesz)
			bo.PutUint64(raw[o+40:], p.Memsz)
			bo.PutUint64(raw[o+48:], p.Align)
		}
		var phdrPtr uintptr
		if len(raw) > 0 {
			phdrPtr = uintptr(unsafe.Pointer(&raw[0]))
		}
		*(*uintptr)(unsafe.Pointer(&info[0])) = uintptr(im.Bias)
		*(*uintptr)(unsafe.Pointer(&info[8])) = hostabi.CString(im.Eng.Path)
		*(*uintptr)(unsafe.Pointer(&info[16])) = phdrPtr
		*(*uint16)(unsafe.Pointer(&info[24])) = uint16(len(progs))
		info[28] = 0 // padding
		rc := hostabi.CallGuestInt3(cb, uintptr(unsafe.Pointer(&info[0])), 64, data)
		if rc != 0 {
			// glibc/bionic dl_iterate_phdr returns the callback's non-zero
			// value (the caller uses it to confirm the match). Dropping it made
			// _Unwind_Find_FDE read as "not found" even on a hit.
			return int(rc)
		}
	}
	return 0
}
