package hostabi

import (
	"sync"
	"unsafe"
)

// setjmp and longjmp cannot be Go functions behind thunks: setjmp returns
// twice, and longjmp abandons the frames between it and the setjmp. They are
// raw machine code bound straight into the guest's GOT (like the gl*/egl*
// functions), operating on bionic's x86_64 jmp_buf (11 longs, 88 bytes; only
// the first 72 are used here). The engine uses them in third-party code paths
// (for example FMOD and image decoders) that report errors by longjmp.
//
// The signal mask is not saved or restored: guest code never has signals
// delivered to it by this runtime, so sigsetjmp/siglongjmp reduce to the same
// thing.
//
//	setjmp(env=%rdi):
//		movq (%rsp),%rax
//		movq %rbx,0(%rdi); movq %rbp,8(%rdi)
//		movq %r12,16(%rdi); movq %r13,24(%rdi)
//		movq %r14,32(%rdi); movq %r15,40(%rdi)
//		leaq 8(%rsp),%rdx; movq %rdx,48(%rdi)   // SP after return
//		movq %rax,56(%rdi)                      // return address
//		stmxcsr 64(%rdi); fnstcw 68(%rdi)
//		xorl %eax,%eax; ret
//	longjmp(env=%rdi, val=%esi):
//		movl %esi,%eax; testl %eax,%eax; jnz 1f; incl %eax   // never returns 0
//	1:	ldmxcsr 64(%rdi); fldcw 68(%rdi)
//		movq 0..40(%rdi) -> %rbx,%rbp,%r12..%r15
//		movq 48(%rdi),%rsp; jmpq *56(%rdi)
var jmpCode = []byte{
	0x48, 0x8b, 0x04, 0x24, 0x48, 0x89, 0x1f, 0x48, 0x89, 0x6f, 0x08, 0x4c,
	0x89, 0x67, 0x10, 0x4c, 0x89, 0x6f, 0x18, 0x4c, 0x89, 0x77, 0x20, 0x4c,
	0x89, 0x7f, 0x28, 0x48, 0x8d, 0x54, 0x24, 0x08, 0x48, 0x89, 0x57, 0x30,
	0x48, 0x89, 0x47, 0x38, 0x0f, 0xae, 0x5f, 0x40, 0xd9, 0x7f, 0x44, 0x31,
	0xc0, 0xc3, 0x89, 0xf0, 0x85, 0xc0, 0x75, 0x02, 0xff, 0xc0, 0x0f, 0xae,
	0x57, 0x40, 0xd9, 0x6f, 0x44, 0x48, 0x8b, 0x1f, 0x48, 0x8b, 0x6f, 0x08,
	0x4c, 0x8b, 0x67, 0x10, 0x4c, 0x8b, 0x6f, 0x18, 0x4c, 0x8b, 0x77, 0x20,
	0x4c, 0x8b, 0x7f, 0x28, 0x48, 0x8b, 0x67, 0x30, 0xff, 0x67, 0x38,
}

const (
	setjmpOff  = 0
	longjmpOff = 0x32
)

var (
	jmpOnce sync.Once
	jmpBase uintptr
)

// rawCodeSymbol returns hand-written machine code implementing name, for the
// few libc functions that cannot be thunked to Go.
func rawCodeSymbol(name string) (uintptr, bool) {
	var off uintptr
	switch name {
	case "setjmp", "_setjmp", "sigsetjmp":
		off = setjmpOff
	case "longjmp", "_longjmp", "siglongjmp":
		off = longjmpOff
	default:
		return 0, false
	}
	jmpOnce.Do(func() {
		b := codeArena(len(jmpCode))
		copy(b, jmpCode)
		jmpBase = uintptr(unsafe.Pointer(&b[0]))
		arenaMakeExec()
	})
	return jmpBase + off, true
}
