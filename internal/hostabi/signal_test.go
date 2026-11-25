package hostabi

import (
	"testing"
	"unsafe"
)

// bionic's LP64 struct sigaction is 32 bytes and sigset_t is 8; writing more
// overran the guest's stack (curl's SIGPIPE save/restore tripped the stack
// protector). Every byte past those sizes must be left alone.
func TestSignalStructSizes(t *testing.T) {
	mem, unmap := pin(t)
	defer unmap()
	guard := func(p uintptr, n int) {
		for i := 0; i < n; i++ {
			*(*byte)(unsafe.Pointer(p + uintptr(i))) = 0xAA
		}
	}
	checkGuard := func(what string, p uintptr, from, to int) {
		for i := from; i < to; i++ {
			if b := *(*byte)(unsafe.Pointer(p + uintptr(i))); b != 0xAA {
				t.Fatalf("%s wrote byte %d (= %#x) past its struct", what, i, b)
			}
		}
	}

	guard(mem, 256)
	goSigemptyset(mem)
	checkGuard("sigemptyset", mem, bionicSigsetSize, 256)
	guard(mem, 256)
	goSigfillset(mem)
	checkGuard("sigfillset", mem, bionicSigsetSize, 256)
	if goSigismember(mem, 13) != 1 || goSigismember(mem, 64) != 1 {
		t.Fatal("sigismember on a full set")
	}

	// sigaction(SIGPIPE, NULL, &old) then sigaction(SIGPIPE, &ign, NULL) and
	// read it back, as curl does.
	old, act := mem, mem+512
	guard(old, 256)
	if r := goSigaction(13, 0, old); r != 0 {
		t.Fatalf("sigaction: %d", r)
	}
	checkGuard("sigaction oldact", old, bionicSigactionSize, 256)
	for i := 0; i < bionicSigactionSize; i++ {
		*(*byte)(unsafe.Pointer(act + uintptr(i))) = 0
	}
	*(*uintptr)(unsafe.Pointer(act + 8)) = 1 // SIG_IGN
	goSigaction(13, act, 0)
	goSigaction(13, 0, old)
	if h := *(*uintptr)(unsafe.Pointer(old + 8)); h != 1 {
		t.Fatalf("sigaction did not report the installed handler: %#x", h)
	}
	if prev := goSignal(13, 0); prev != 1 {
		t.Fatalf("signal() returned %#x, want the previous SIG_IGN", prev)
	}
	if goSigaction(0, 0, old) != -1 || goSigaction(65, 0, old) != -1 {
		t.Fatal("sigaction accepted an invalid signal number")
	}
}
