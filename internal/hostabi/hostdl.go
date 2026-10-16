package hostabi

import (
	"debug/elf"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// This file reaches the *host's* C libraries from pure Go. The runtime makes no
// cgo calls, but the rbxrt binary is dynamically linked (the C library is
// mapped because the Go runtime uses it for DNS and user lookup), so the real
// dlopen/dlsym are present in this process. We find libc in /proc/self/maps,
// read its own dynamic symbol table to locate those two entry points, and then
// call them through the same guest-call trampoline used everywhere else.
//
// The graphics layer builds on this: it dlopen()s the host's libEGL, libGLESv2
// and libX11 and binds the engine's imports to the real implementations. The
// platform is not emulated and nothing is faked — the engine gets Mesa's
// driver, the same one a native Linux program would.

const (
	rtldLazy   = 0x1
	rtldNow    = 0x2
	rtldGlobal = 0x100
)

var (
	hostDlOnce   sync.Once
	hostDlopenFn uintptr
	hostDlsymFn  uintptr
)

// libcIdentity returns the path of the mapped C library and its load bias.
// /proc/self/maps is address-ordered, so the first matching line is the lowest
// mapping of the library.
func libcIdentity() (path string, base uintptr, ok bool) {
	data, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return "", 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "libc.so.6") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			continue
		}
		off, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil {
			continue
		}
		return fields[len(fields)-1], uintptr(start) - uintptr(off), true
	}
	return "", 0, false
}

// libcSymbol resolves a dynamic symbol in the mapped C library.
func libcSymbol(name string) uintptr {
	path, base, ok := libcIdentity()
	if !ok {
		return 0
	}
	f, err := elf.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	syms, err := f.DynamicSymbols()
	if err != nil {
		return 0
	}
	for _, s := range syms {
		if s.Name == name {
			return base + uintptr(s.Value)
		}
	}
	return 0
}

func initHostDl() {
	hostDlOnce.Do(func() {
		hostDlopenFn = libcSymbol("dlopen")
		hostDlsymFn = libcSymbol("dlsym")
	})
}

// hostDlAvailable reports whether the host's dlopen could be located. It is
// false when the process is statically linked (no libc mapped), which is the
// one situation the graphics layer cannot work around.
func hostDlAvailable() bool {
	initHostDl()
	return hostDlopenFn != 0
}

// callCOnGuestStack and callCDirect are implemented in abi_amd64.s. They call a
// host C function with up to nine integer arguments, on and off the guest stack
// respectively.
//
//go:noescape
func callCOnGuestStack(gs *guestStack, fn uintptr, args *[9]uintptr) uintptr

//go:noescape
func callCDirect(fn uintptr, args *[9]uintptr) uintptr

// hostDlopen loads a host shared object into the process and returns its handle
// (0 on failure). It runs the call on the guest stack, where a driver's
// initialization has room to breathe.
func hostDlopen(name string, flags uintptr) uintptr {
	initHostDl()
	if hostDlopenFn == 0 {
		return 0
	}
	var args [9]uintptr
	args[0] = CString(name)
	args[1] = flags
	return callHostC(hostDlopenFn, &args)
}

// hostDlsym resolves a symbol in a handle returned by hostDlopen.
func hostDlsym(handle uintptr, name string) uintptr {
	initHostDl()
	if hostDlsymFn == 0 {
		return 0
	}
	var args [9]uintptr
	args[0] = handle
	args[1] = CString(name)
	return callHostC(hostDlsymFn, &args)
}

// callHostC calls a host C function with up to nine integer arguments, switching
// to the guest stack at the top level exactly as invokeGuest does. It is the Go
// face of callCOnGuestStack/callCDirect.
func callHostC(fn uintptr, args *[9]uintptr) uintptr {
	g := getg()
	gs := getGuestStack(g)
	if gs.depth > 0 {
		return callCDirect(fn, args)
	}
	gs.depth = 1
	gs.sp = gs.top &^ 15
	oldLo := *(*uintptr)(unsafe.Pointer(g))
	oldHi := *(*uintptr)(unsafe.Pointer(g + 8))
	*(*uintptr)(unsafe.Pointer(g)) = gs.base
	*(*uintptr)(unsafe.Pointer(g + 8)) = gs.top
	o0, o1 := zeroGuards(g)
	r := callCOnGuestStack(gs, fn, args)
	restoreGuards(g, o0, o1)
	*(*uintptr)(unsafe.Pointer(g)) = oldLo
	*(*uintptr)(unsafe.Pointer(g + 8)) = oldHi
	gs.depth = 0
	return r
}
