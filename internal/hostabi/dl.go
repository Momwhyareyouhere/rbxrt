package hostabi

import (
	"fmt"
	"os"
	"unsafe"
)

// The dynamic-linker functions (dlopen & friends) are implemented by the loader,
// which owns the process-wide symbol namespace. hostabi exposes hooks that the
// loader installs before any guest code runs.

// DLHooks is the set of callbacks the loader provides for guest dl* calls.
type DLHooks struct {
	Open        func(name string, flags uintptr) uintptr
	Sym         func(handle uintptr, name string) uintptr
	Close       func(handle uintptr) int
	Error       func() string
	Addr        func(addr uintptr, info uintptr) int
	IteratePhdr func(cb uintptr, data uintptr) int
}

var dlHooks DLHooks

// SetDLHooks installs the loader's dynamic-linker callbacks.
func SetDLHooks(h DLHooks) { dlHooks = h }

var lastDLError string

func setDLError(msg string) {
	lastDLError = msg
}

//go:noinline
func goDlopen(name, flags uintptr) uintptr {
	if dlHooks.Open == nil {
		setDLError("rbxrt: dlopen called before the loader installed its hooks")
		return 0
	}
	return dlHooks.Open(cstr(name), flags)
}

//go:noinline
func goDlsym(handle, name uintptr) uintptr {
	if dlHooks.Sym == nil {
		setDLError("rbxrt: dlsym called before the loader installed its hooks")
		return 0
	}
	return dlHooks.Sym(handle, cstr(name))
}

//go:noinline
func goDlclose(handle uintptr) int {
	if dlHooks.Close == nil {
		return 0
	}
	return dlHooks.Close(handle)
}

//go:noinline
func goDlerror() uintptr {
	var msg string
	if dlHooks.Error != nil {
		msg = dlHooks.Error()
	}
	if msg == "" {
		msg = lastDLError
	}
	if msg == "" {
		return 0
	}
	return cstringConst(msg)
}

//go:noinline
func goDladdr(addr, info uintptr) int {
	if dlHooks.Addr == nil {
		return 0
	}
	return dlHooks.Addr(addr, info)
}

//go:noinline
func goDlIteratePhdr(cb, data uintptr) int {
	if dlHooks.IteratePhdr == nil {
		return 0
	}
	return dlHooks.IteratePhdr(cb, data)
}

// FindLibrary is a convenience used by guests looking for platform libs; not
// implemented, fails loudly.
//
//go:noinline
func goFindLibrary(name uintptr) uintptr {
	fmt.Fprintf(os.Stderr, "rbxrt: fatal: guest called unimplemented host symbol \"android_dlopen_ext/%s\"\n", cstr(name))
	os.Exit(70)
	return 0
}

var _ = unsafe.Pointer(nil)
