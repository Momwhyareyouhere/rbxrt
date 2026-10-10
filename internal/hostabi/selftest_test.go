package hostabi

import (
	"syscall"
	"testing"
	"unsafe"
)

// pin allocates a page of non-GC-managed zeroed memory and returns a pointer to
// it. Host functions take raw guest addresses (uintptr), so test buffers must
// not live in the Go heap: a Go-allocated []byte handed over as a uintptr can be
// collected while only a uintptr reference remains, which would make these tests
// flaky. mmap mirrors the loader, which maps guest memory itself.
func pin(t *testing.T) (uintptr, func()) {
	t.Helper()
	p, err := syscall.Mmap(-1, 0, 4096,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	return uintptr(unsafe.Pointer(&p[0])), func() { syscall.Munmap(p) }
}

// putCString writes a NUL-terminated string into pinned memory and returns its
// address.
func putCString(t *testing.T, s string) (uintptr, func()) {
	t.Helper()
	p, unmap := pin(t)
	for i := 0; i < len(s); i++ {
		*(*byte)(unsafe.Pointer(p + uintptr(i))) = s[i]
	}
	*(*byte)(unsafe.Pointer(p + uintptr(len(s)))) = 0
	return p, unmap
}

func cstrAt(p uintptr, max int) string {
	b := []byte{}
	for i := 0; i < max; i++ {
		c := *(*byte)(unsafe.Pointer(p + uintptr(i)))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

func lookupOrFatal(t *testing.T, name string) uintptr {
	t.Helper()
	a, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q): not implemented", name)
	}
	return a
}

// TestHostFunctionsViaThunks exercises the generated trampolines the way guest
// code will: it looks up a symbol and calls it through its thunk with SysV
// argument registers. If the register translation or the ABIInternal call is
// wrong these tests fail immediately.
func TestHostFunctionsViaThunks(t *testing.T) {
	// memset(buf, 'A', 5)
	p, unmapP := pin(t)
	defer unmapP()
	memset := lookupOrFatal(t, "memset")
	CallHostRaw(memset, p, 'A', 5)
	for i := 0; i < 5; i++ {
		if got := *(*byte)(unsafe.Pointer(p + uintptr(i))); got != 'A' {
			t.Fatalf("memset: byte %d = %#x, want 'A'", i, got)
		}
	}
	if got := *(*byte)(unsafe.Pointer(p + 5)); got != 0 {
		t.Fatalf("memset wrote past n: %#x", got)
	}

	// strlen(buf) == 5
	strlen := lookupOrFatal(t, "strlen")
	if n := CallHostRaw(strlen, p, 0, 0); n != 5 {
		t.Fatalf("strlen: got %d want 5", n)
	}

	// strcpy(dst, "hello world")
	dp, unmapD := pin(t)
	defer unmapD()
	src, unmapS := putCString(t, "hello world")
	defer unmapS()
	strcpy := lookupOrFatal(t, "strcpy")
	CallHostRaw(strcpy, dp, src, 0)
	if got := cstrAt(dp, 16); got != "hello world" {
		t.Fatalf("strcpy: got %q", got)
	}

	// memcmp equal / unequal (lexicographic, returns difference)
	memcmp := lookupOrFatal(t, "memcmp")
	less, unmapL := putCString(t, "AAAAA")
	defer unmapL()
	greater, unmapG := putCString(t, "AAAAZ")
	defer unmapG()
	if r := CallHostRaw(memcmp, less, less, 5); r != 0 {
		t.Fatalf("memcmp equal: got %d", r)
	}
	if r := int(int32(CallHostRaw(memcmp, less, greater, 5))); r >= 0 {
		t.Fatalf("memcmp less-vs-greater: got %d want <0", r)
	}
	if r := int(int32(CallHostRaw(memcmp, greater, less, 5))); r <= 0 {
		t.Fatalf("memcmp greater-vs-less: got %d want >0", r)
	}

	// atoi("  -42xyz") == -42
	num, unmapN := putCString(t, "  -42xyz")
	defer unmapN()
	atoi := lookupOrFatal(t, "atoi")
	if n := int(int32(CallHostRaw(atoi, num, 0, 0))); n != -42 {
		t.Fatalf("atoi: got %d want -42", n)
	}

	// getpagesize -> 4096 (returns a value without reading memory)
	gp := lookupOrFatal(t, "getpagesize")
	if n := CallHostRaw(gp, 0, 0, 0); n == 0 {
		t.Fatal("getpagesize: returned 0")
	}
}

// TestDataSymbolsPresent checks the data objects the genuine lib imports are
// all available.
func TestDataSymbolsPresent(t *testing.T) {
	want := []string{
		"__sF", "stdin", "stdout", "stderr", "environ",
		"__stack_chk_guard", "optarg", "optind", "timezone", "tzname",
		"daylight", "in6addr_any", "in6addr_loopback",
	}
	for _, n := range want {
		if _, ok := DataSymbol(n); !ok {
			t.Errorf("data symbol %q missing", n)
		}
	}
}

// TestTrapThunkDistinct documents that unknown symbols bind to a distinct trap.
// We do not call them here: a trap thunk aborts the process.
func TestTrapThunkDistinct(t *testing.T) {
	a1, ok1 := Lookup("definitely_not_a_real_symbol_123")
	a2, ok2 := Lookup("definitely_not_a_real_symbol_456")
	if ok1 || ok2 {
		t.Fatal("unknown symbols should not report implemented")
	}
	if a1 == a2 {
		t.Fatal("trap thunks for different names must differ")
	}
}
