package hostabi

import (
	"syscall"
	"testing"
	"unsafe"
)

// fiberCallStub is guest-style code that does what the engine's task fibers do
// around a host call: it switches SP to a stack it allocated itself, calls a
// host thunk there, and switches back.
//
//	fiber(thunk=rdi, fiberTop=rsi, arg=rdx):
//		push %rbp; mov %rsp,%rbp
//		mov %rsp,%rax; mov %rsi,%rsp     // switch to the fiber stack
//		push %rax; sub $8,%rsp           // keep the old SP; align
//		mov %rdi,%rcx; mov %rdx,%rdi
//		call *%rcx                       // host call from the fiber
//		add $8,%rsp; pop %rcx; mov %rcx,%rsp
//		pop %rbp; ret
var fiberCallStub = []byte{
	0x55, 0x48, 0x89, 0xe5, 0x48, 0x89, 0xe0, 0x48, 0x89, 0xf4, 0x50, 0x48,
	0x83, 0xec, 0x08, 0x48, 0x89, 0xf9, 0x48, 0x89, 0xd7, 0xff, 0xd1, 0x48,
	0x83, 0xc4, 0x08, 0x59, 0x48, 0x89, 0xcc, 0x5d, 0xc3,
}

var fiberHostSP uintptr

// fiberHost is the host function called from the fiber. It records where it
// ran and makes a real Go syscall, which validates SP against g.stack and
// aborted ("entersyscall inconsistent sp") before thunks moved host calls off
// foreign stacks.
//
//go:noinline
func fiberHost(arg uintptr) uintptr {
	fiberHostSP = currentSP()
	if syscall.Getpid() <= 0 {
		return 0
	}
	return arg + 1
}

func TestHostCallFromForeignStack(t *testing.T) {
	stub := codeArena(len(fiberCallStub))
	copy(stub, fiberCallStub)
	thunk := mkThunk(funcAddr(fiberHost))
	arenaMakeExec()

	fiber, err := syscall.Mmap(-1, 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatalf("mmap fiber stack: %v", err)
	}
	defer syscall.Munmap(fiber)
	fiberTop := (uintptr(unsafe.Pointer(&fiber[0])) + uintptr(len(fiber))) &^ 15

	got := CallGuestInt3(uintptr(unsafe.Pointer(&stub[0])), thunk, fiberTop, 41)
	if got != 42 {
		t.Fatalf("host call from fiber returned %d, want 42", got)
	}
	gs := getGuestStack(getg())
	if fiberHostSP >= gs.base+0x2000000 || fiberHostSP < gs.base {
		t.Fatalf("host code ran at SP %#x, want inside the reserved area [%#x,%#x)",
			fiberHostSP, gs.base, gs.base+0x2000000)
	}
	if slot := *(*uintptr)(unsafe.Pointer(gs.base - guestAltStackSlot)); slot != 0 {
		t.Fatalf("reserved-area level not released: slot=%#x", slot)
	}

	// A host call from the thread's own guest stack must still run in place.
	fiberHostSP = 0
	if got := CallGuestInt3(thunk, 1, 0, 0); got != 2 {
		t.Fatalf("direct host call returned %d, want 2", got)
	}
	if fiberHostSP < gs.base+0x2000000 || fiberHostSP >= gs.top {
		t.Fatalf("direct host call ran at SP %#x, outside the normal guest stack", fiberHostSP)
	}
}

// setjmpCallStub calls setjmp(env) and, on its first return, longjmp(env, 7);
// it returns whatever the second return of setjmp delivers.
//
//	stub(setjmp=rdi, longjmp=rsi, env=rdx):
//		push %rbx; push %r12; push %r13; sub $16,%rsp   // keep 16-alignment
//		mov %rdi,%rbx; mov %rsi,%r12; mov %rdx,%r13
//		mov %r13,%rdi; call *%rbx                         // setjmp(env)
//		test %eax,%eax; jnz 1f
//		mov %r13,%rdi; mov $7,%esi; call *%r12            // longjmp(env, 7)
//	1:	add $16,%rsp; pop %r13; pop %r12; pop %rbx; ret
var setjmpCallStub = []byte{
	0x53, 0x41, 0x54, 0x41, 0x55, 0x48, 0x83, 0xec, 0x10, 0x48, 0x89, 0xfb,
	0x49, 0x89, 0xf4, 0x49, 0x89, 0xd5, 0x4c, 0x89, 0xef, 0xff, 0xd3, 0x85,
	0xc0, 0x75, 0x0b, 0x4c, 0x89, 0xef, 0xbe, 0x07, 0x00, 0x00, 0x00, 0x41,
	0xff, 0xd4, 0x48, 0x83, 0xc4, 0x10, 0x41, 0x5d, 0x41, 0x5c, 0x5b, 0xc3,
}

func TestSetjmpLongjmp(t *testing.T) {
	sj, ok := Lookup("setjmp")
	lj, ok2 := Lookup("longjmp")
	if !ok || !ok2 {
		t.Fatal("setjmp/longjmp not implemented")
	}
	stub := codeArena(len(setjmpCallStub))
	copy(stub, setjmpCallStub)
	arenaMakeExec()
	env, unmap := pin(t)
	defer unmap()
	if got := CallGuestInt3(uintptr(unsafe.Pointer(&stub[0])), sj, lj, env); got != 7 {
		t.Fatalf("setjmp returned %d after longjmp(env, 7), want 7", got)
	}
}

// fiber3Stub is fiberCallStub for a three-argument call, with %al = 0 as a
// variadic call requires:
//
//	fiber3(thunk=rdi, fiberTop=rsi, a=rdx, b=rcx, c=r8): thunk(a, b, c)
var fiber3Stub = []byte{
	0x55, 0x48, 0x89, 0xe5, 0x48, 0x89, 0xe0, 0x48, 0x89, 0xf4, 0x50, 0x48,
	0x83, 0xec, 0x08, 0x49, 0x89, 0xfb, 0x48, 0x89, 0xd7, 0x48, 0x89, 0xce,
	0x4c, 0x89, 0xc2, 0x31, 0xc0, 0x41, 0xff, 0xd3, 0x48, 0x83, 0xc4, 0x08,
	0x59, 0x48, 0x89, 0xcc, 0x5d, 0xc3,
}

// The variadic trampoline (sprintf and friends) takes its own path into Go
// (variadicEntry), which must move off a fiber stack just like a plain thunk.
func TestVariadicCallFromForeignStack(t *testing.T) {
	sprintf, ok := Lookup("sprintf")
	if !ok {
		t.Fatal("sprintf not implemented")
	}
	stub := codeArena(len(fiber3Stub))
	copy(stub, fiber3Stub)
	arenaMakeExec()
	fiber, err := syscall.Mmap(-1, 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatalf("mmap fiber stack: %v", err)
	}
	defer syscall.Munmap(fiber)
	fiberTop := (uintptr(unsafe.Pointer(&fiber[0])) + uintptr(len(fiber))) &^ 15
	buf, unmapB := pin(t)
	defer unmapB()
	format, unmapF := putCString(t, "fiber %d")
	defer unmapF()

	n := CallGuestInt8(uintptr(unsafe.Pointer(&stub[0])), sprintf, fiberTop, buf, format, 42, 0, 0, 0)
	if got := cstrAt(buf, 64); got != "fiber 42" || n != 8 {
		t.Fatalf("sprintf from fiber: %q (n=%d), want \"fiber 42\" (8)", got, n)
	}
	gs := getGuestStack(getg())
	if slot := *(*uintptr)(unsafe.Pointer(gs.base - guestAltStackSlot)); slot != 0 {
		t.Fatalf("reserved-area level not released: slot=%#x", slot)
	}
}
