package hostabi

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// --- environment backing -------------------------------------------------

var (
	environSlice []uintptr // char* for each "KEY=VALUE", NUL terminated
	environVar   uintptr   // char** pointing at environSlice
	// environSym is the storage the dynamic symbol "environ" denotes. C declares
	// it as `extern char **environ`, so the symbol's value is the address of a
	// variable holding the array pointer, not the array itself.
	environSym  uintptr
	environPtrs []uintptr
)

func envGet(name string) uintptr {
	v, ok := os.LookupEnv(name)
	if !ok {
		return 0
	}
	b := append([]byte(v), 0)
	pin := append(envPin, b)
	envPin = pin
	return uintptr(unsafe.Pointer(&b[0]))
}

var envPin [][]byte

var (
	// guestTCB points to the memory region used as the guest's Thread Control Block
	// (TCB) when %fs is switched to the guest TCB. The TCB layout mimics the
	// beginning of pthread_internal_t: we need at least the canary at offset 0x28.
	guestTCB uintptr
	// goTLSBase is the value of the FS base register when pointing to the Go TLS
	// (i.e., the address of the g struct). It is initialized once at startup.
	goTLSBase uintptr
)

// initGuestTCB allocates a page for the guest TCB and stores the stack canary
// at offset 0x28. It must be called before any guest code runs.
func initGuestTCB() {
	// Allocate one page of anonymous memory for the TCB.
	tcbSize := uintptr(syscall.Getpagesize())
	p, err := syscall.Mmap(-1, 0, int(tcbSize),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		// Should not happen; if it does, we cannot continue.
		fmt.Fprintln(os.Stderr, "rbxrt: fatal: failed to map guest TCB")
		os.Exit(1)
	}
	guestTCB = uintptr(unsafe.Pointer(&p[0]))
	// Store the stack canary at the expected offset.
	*(*uint64)(unsafe.Pointer(guestTCB + 0x28)) = stackGuard
}

func envSet(name, value string, overwrite bool) bool {
	if !overwrite {
		if _, ok := os.LookupEnv(name); ok {
			return true
		}
	}
	if err := os.Setenv(name, value); err != nil {
		return false
	}
	rebuildEnviron()
	return true
}

func envUnset(name string) {
	os.Unsetenv(name)
	rebuildEnviron()
}

// rebuildEnviron refreshes the char** environ array exposed to guests.
func rebuildEnviron() {
	env := os.Environ()
	environSlice = environSlice[:0]
	for _, kv := range env {
		b := append([]byte(kv), 0)
		envPin = append(envPin, b)
		environSlice = append(environSlice, uintptr(unsafe.Pointer(&b[0])))
	}
	environSlice = append(environSlice, 0) // NULL terminator
	environPtrs = environSlice
	environVar = uintptr(unsafe.Pointer(&environPtrs[0]))
	environSym = environVar
}

// --- misc ----------------------------------------------------------------

var processStart = time.Now()

// monotonicNowNanos returns nanoseconds from a process-relative monotonic
// clock. Only for callers that genuinely want elapsed process time (POSIX
// clock()). Anything the guest may hand back to the kernel must use the
// kernel-frame clock below instead.
func monotonicNowNanos() int64 { return int64(time.Since(processStart)) }

// hostClockOffset[id] = (host kernel clock id in ns) - (process-relative
// monotonic reading), sampled once at startup. Guest code reads these clocks
// with clock_gettime and then hands the *absolute* result straight to real
// kernel syscalls: FUTEX_WAIT_BITSET timeouts, timerfd_settime(TFD_TIMER_ABSTIME)
// and clock_nanosleep(TIMER_ABSTIME). The kernel evaluates those against its own
// clock, so a process-relative value (a few seconds since exec) is already long
// expired against the real CLOCK_MONOTONIC (seconds since boot) - every timed
// wait returns ETIMEDOUT at once and the caller spins. Go's monotonic clock is
// the same CLOCK_MONOTONIC the kernel uses, so one offset per clock is exact
// and the clock_gettime hot path needs no syscall of its own.
var hostClockOffset [8]int64

func init() {
	for _, id := range [...]int{1 /*MONOTONIC*/, 4 /*MONOTONIC_RAW*/, 6 /*MONOTONIC_COARSE*/, 7 /*BOOTTIME*/} {
		var ts syscall.Timespec
		if _, _, e := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME,
			uintptr(id), uintptr(unsafe.Pointer(&ts)), 0); e == 0 {
			hostClockOffset[id] = int64(ts.Sec)*1e9 + int64(ts.Nsec) - int64(time.Since(processStart))
		}
	}
}

// hostMonotonicNowNanos returns nanoseconds on monotonic clock clockid in the
// host kernel's frame (what the guest must observe).
func hostMonotonicNowNanos(clockid int) int64 {
	return int64(time.Since(processStart)) + hostClockOffset[clockid&7]
}

func getauxval(kind uintptr) uintptr {
	switch kind {
	case 6: // AT_PAGESZ
		return uintptr(os.Getpagesize())
	case 17: // AT_CLKTCK
		return 100
	default:
		return 0
	}
}

// goStringOf reads a C string pointer into a Go string (helper for logging).
func goStringOf(p uintptr) string { return cstr(p) }

// envLookupString is used by tests.
func envLookupString(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	return v, ok
}

func getg() uintptr {
	return *(*uintptr)(unsafe.Pointer(readFSbase() - 8))
}
