package hostabi

import (
	"os"
	"strings"
	"time"
	"unsafe"
)

// --- environment backing -------------------------------------------------

var (
	environSlice []uintptr // char* for each "KEY=VALUE", NUL terminated
	environVar   uintptr   // char** pointing at environSlice
	environPtrs  []uintptr
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
}

// --- misc ----------------------------------------------------------------

var processStart = time.Now()

// monotonicNowNanos returns nanoseconds from a process-relative monotonic
// clock. Guest code uses CLOCK_MONOTONIC only for measuring elapsed time.
func monotonicNowNanos() int64 { return int64(time.Since(processStart)) }

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

var _ = strings.TrimSpace
