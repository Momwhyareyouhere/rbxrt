#include "textflag.h"

// HOSTCALL(target) calls the Go function whose ABIInternal entry is stored in
// the variable target, with its arguments already in AX/BX/CX and g in R14,
// and with SP 8 (mod 16). It is the assembly twin of thunkCallTail (thunk.go):
// when SP is outside [g.stack.lo, g.stack.hi) - the engine called in from one
// of its own fiber stacks - the call runs on the reserved area at the bottom
// of this thread's guest stack instead, one 4 MiB level per nesting, tracked
// in the guard slot at lo-40. Clobbers DX, SI, DI, R11, R12, R13; the guest's
// values of the callee-saved ones must already be pushed. Each function may
// use it once (its labels are function-scoped).
#define HOSTCALL(target) \
	MOVQ 0(R14), R12; \
	CMPQ SP, R12; \
	JCS hc_foreign; \
	CMPQ SP, 8(R14); \
	JCC hc_foreign; \
	MOVQ target, R11; \
	SUBQ $200, SP; \
	CALL R11; \
	ADDQ $200, SP; \
	JMP hc_done; \
hc_foreign: \
	MOVQ -40(R12), R13; \
	MOVQ R13, DX; \
	TESTQ DX, DX; \
	JNZ hc_have; \
	LEAQ 0x2000000(R12), DX; \
hc_have: \
	LEAQ -0x400000(DX), SI; \
	CMPQ SI, R12; \
	JHI hc_ok; \
	INT $3; \
hc_ok: \
	MOVQ SI, -40(R12); \
	MOVQ SP, DI; \
	MOVQ DX, SP; \
	PUSHQ R13; \
	PUSHQ DI; \
	MOVQ target, R11; \
	SUBQ $208, SP; \
	CALL R11; \
	ADDQ $208, SP; \
	POPQ DI; \
	POPQ R13; \
	MOVQ 0(R14), R12; \
	MOVQ R13, -40(R12); \
	MOVQ DI, SP; \
hc_done:

// func callGuestVoid(fn uintptr)
//
// Invokes a guest function that takes no arguments and returns nothing (a
// DT_INIT / DT_INIT_ARRAY entry point). The guest uses the SysV ABI, which
// requires %rsp to be 16-byte aligned immediately before a CALL, so that the
// callee enters with %rsp == 8 (mod 16). We cannot assume anything about the
// alignment our Go caller leaves: an earlier version simply did `SUBQ $8` and
// silently produced a misaligned guest entry once the caller's frame changed,
// which the engine eventually hit as a #GP on an aligned SSE access (movaps).
//
// Instead we save the frame pointer, force alignment with ANDQ, and restore.
// BP is callee-saved under SysV, so the guest preserves it across the call.
TEXT ·callGuestVoid(SB), NOSPLIT|NOFRAME, $0-8
	MOVQ fn+0(FP), AX
	PUSHQ BP
	MOVQ SP, BP
	ANDQ $-16, SP
	CALL AX
	MOVQ BP, SP
	POPQ BP
	RET

// func callGuest0(fn uintptr) uintptr
//
// Invokes a guest function with no arguments and returns its integer result.
// Used for IFUNC/IRELATIVE resolvers, which return the resolved address.
TEXT ·callGuest0(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ fn+0(FP), AX
	PUSHQ BP
	MOVQ SP, BP
	ANDQ $-16, SP
	CALL AX
	MOVQ BP, SP
	POPQ BP
	MOVQ AX, ret+8(FP)
	RET

// func callGuestInt3(fn, a, b, c uintptr) int
//
// Invokes a guest function with three integer arguments and returns its result.
// Used for callbacks such as dl_iterate_phdr's visitor and bsearch comparators.
TEXT ·callGuestInt3(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ fn+0(FP), R10
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	PUSHQ BP
	MOVQ SP, BP
	ANDQ $-16, SP
	CALL R10
	MOVQ BP, SP
	POPQ BP
	MOVQ AX, ret+32(FP)
	RET

// func callGuestVoid1(fn, a uintptr)
//
// Invokes a guest function with one integer argument and no return value. Used
// for atexit-style destructors: void (*)(void*).
TEXT ·callGuestVoid1(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ fn+0(FP), AX
	MOVQ a+8(FP), DI
	PUSHQ BP
	MOVQ SP, BP
	ANDQ $-16, SP
	CALL AX
	MOVQ BP, SP
	POPQ BP
	RET

// func callHostRaw(fn, a, b, c uintptr) uintptr
//
// Calls a host function through its thunk as guest code would (SysV argument
// registers). It exists so the runtime can self-test the host library and is
// also used by the loader's symbol-resolution checks.
TEXT ·callHostRaw(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ fn+0(FP), R10
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	MOVQ -8(FS), R14
	SUBQ $8, SP
	CALL R10
	ADDQ $8, SP
	MOVQ AX, ret+32(FP)
	RET

// func clobberCalleeSaved()
//
// A deliberately hostile thunk target for tests: it destroys every SysV
// callee-saved register except the frame pointer (which the Go assembler
// requires be left alone). Saving and restoring them is the host thunk's job,
// so a thunk that forgot is detected by thunkPreserveProbe.
TEXT ·clobberCalleeSaved(SB), NOSPLIT|NOFRAME, $0-0
	XORQ BX, BX
	XORQ R12, R12
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
	RET

// func thunkPreserveProbe(thunk uintptr) uintptr
//
// Drives a host thunk exactly as guest code would, placing sentinels in every
// SysV callee-saved register, then reports which were not preserved as a
// bitmask (bit0 RBX, bit1 RBP, bit2 R12, bit3 R13, bit4 R14, bit5 R15). The
// thunk itself supplies g to the Go target, so the probe only has to make sure
// it is running on a Go thread (which it is).
TEXT ·thunkPreserveProbe(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ thunk+0(FP), R11
	// Save the Go caller's registers we are about to overwrite. Five pushes
	// also leave SP 16-byte aligned for the call into the thunk, which expects
	// the same entry alignment a guest function would see.
	PUSHQ BX
	PUSHQ BP
	PUSHQ R12
	PUSHQ R13
	PUSHQ R15
	MOVQ $0x1111111111111111, BX
	MOVQ $0x2222222222222222, BP
	MOVQ $0x3333333333333333, R12
	MOVQ $0x4444444444444444, R13
	MOVQ $0x5555555555555555, R14
	MOVQ $0x6666666666666666, R15
	XORQ DI, DI // safe SysV arg 1 for real targets
	XORQ SI, SI // safe SysV arg 2
	XORQ DX, DX // safe SysV arg 3
	CALL R11
	XORQ AX, AX
	MOVQ $0x1111111111111111, R10
	CMPQ BX, R10
	JEQ ok0
	ORQ $1, AX
ok0:
	MOVQ $0x2222222222222222, R10
	CMPQ BP, R10
	JEQ ok1
	ORQ $2, AX
ok1:
	MOVQ $0x3333333333333333, R10
	CMPQ R12, R10
	JEQ ok2
	ORQ $4, AX
ok2:
	MOVQ $0x4444444444444444, R10
	CMPQ R13, R10
	JEQ ok3
	ORQ $8, AX
ok3:
	MOVQ $0x5555555555555555, R10
	CMPQ R14, R10
	JEQ ok4
	ORQ $16, AX
ok4:
	MOVQ $0x6666666666666666, R10
	CMPQ R15, R10
	JEQ ok5
	ORQ $32, AX
ok5:
	MOVQ AX, ret+8(FP)
	MOVQ -8(FS), R14 // restore g before returning to Go code
	POPQ R15
	POPQ R13
	POPQ R12
	POPQ BP
	POPQ BX
	RET

	// func readFS28() uint64
	// Reads the 8-byte word at FS:+0x28. Used to test TLS stability.
	TEXT ·readFS28(SB), NOSPLIT|NOFRAME, $0-8
		MOVQ 0x28(FS), AX
		MOVQ AX, ret+0(FP)
		RET

	// func currentSP() uintptr
	// Returns the caller's SP. Used by diagnostic probes that track how much
	// of the guest stack is in use.
	TEXT ·currentSP(SB), NOSPLIT, $0-8
		MOVQ SP, AX
		MOVQ AX, ret+0(FP)
		RET

	// func readFSbase() uintptr
	// Returns the current value of the FS base register. Implemented with the
	// arch_prctl syscall because the Go assembler rejects RDFSBASE. The
	// assembler-managed 16-byte frame holds the out-parameter; manually
	// adjusting SP here (as an earlier version did) makes FP-relative stores
	// land in the wrong place and silently returns 0.
	TEXT ·readFSbase(SB), NOSPLIT, $16-8
		LEAQ 0(SP), SI
		MOVQ $0x1003, DI // ARCH_GET_FS
		MOVQ $158, AX    // SYS_arch_prctl
		SYSCALL
		MOVQ 0(SP), AX
		MOVQ AX, ret+0(FP)
		RET

	// func writeFSbase(addr uintptr)
	// Sets the FS base register to addr.
	TEXT ·writeFSbase(SB), NOSPLIT, $0-8
		MOVQ $0x1002, DI // ARCH_SET_FS
		MOVQ addr+0(FP), SI
		MOVQ $158, AX // SYS_arch_prctl
		SYSCALL
		RET

	// func variadicEntry()
	//
	// The shared body of every variadic host thunk (printf, snprintf, ...).
	// The thunk sets R10 to a small integer identifying the function and JMPs
	// here, so the guest's argument registers are still intact. variadicEntry
	// spills the complete SysV variadic state — the six integer registers, the
	// eight SSE registers and the caller's stack overflow area — into a
	// register-save area, then hands it to goVariadic, which interprets it as a
	// va_list. This is why the host can format a guest's variadic call at all:
	// the thunk cannot know the argument types, but the SysV state is fully
	// self-describing once saved.
	//
	// Layout of the save area (176 bytes):
	//   0..48   DI, SI, DX, CX, R8, R9
	//   48..176 XMM0..XMM7 (16 bytes each)
	// The overflow area is the guest's stack immediately above its return
	// address, i.e. entry SP + 8.
	TEXT ·variadicEntry(SB), NOSPLIT|NOFRAME, $0-0
		MOVQ 0(SP), R11  // guest return address
		MOVQ -8(FS), AX  // g
		MOVQ 0(AX), AX   // g.stack.lo == guest stack base
		MOVQ R11, -16(AX) // record it in this goroutine's capture area
		// Preserve the SysV callee-saved registers the guest relies on.
		PUSHQ BX
		PUSHQ BP
		PUSHQ R12
		PUSHQ R13
		PUSHQ R14
		PUSHQ R15
		MOVQ -8(FS), R14 // load g for the Go callee
		SUBQ $192, SP
		MOVQ SP, R11 // R11 = register-save base
		MOVQ DI, 0(R11)
		MOVQ SI, 8(R11)
		MOVQ DX, 16(R11)
		MOVQ CX, 24(R11)
		MOVQ R8, 32(R11)
		MOVQ R9, 40(R11)
		MOVUPS X0, 48(R11)
		MOVUPS X1, 64(R11)
		MOVUPS X2, 80(R11)
		MOVUPS X3, 96(R11)
		MOVUPS X4, 112(R11)
		MOVUPS X5, 128(R11)
		MOVUPS X6, 144(R11)
		MOVUPS X7, 160(R11)
		LEAQ 248(R11), CX // overflow = entry SP + 8
		MOVQ R10, AX      // Go arg 1: function id
		MOVQ R11, BX      // Go arg 2: register-save base
		// Load goVariadic's ABIInternal entry from a Go variable. A direct
		// symbol reference in assembly would bind to the ABI0 wrapper, whose
		// stack convention does not match the register arguments we pass.
		// Go arg 3 is already in CX.
		HOSTCALL(·variadicTarget(SB))
		ADDQ $192, SP
		POPQ R15
		POPQ R14
		POPQ R13
		POPQ R12
		POPQ BP
		POPQ BX
		RET

	// func variadicEntryAddr() uintptr
	// Returns the raw ABI0 body of variadicEntry, which is the correct JMP
	// target for a variadic thunk. Referencing it from Go would instead yield
	// the autogenerated wrapper (with an extra stack frame), so the address is
	// published through this asm getter.
	TEXT ·variadicEntryAddr(SB), NOSPLIT, $0-8
		LEAQ ·variadicEntry(SB), AX
		MOVQ AX, ret+0(FP)
		RET

	// func jniEntry()
	//
	// The shared body of every JNI interface-table trampoline. The trampoline
	// loads the JNI function id into R10 and jumps here with the guest's SysV
	// argument registers (DI, SI, DX, CX, R8, R9) still intact. jniEntry saves
	// those six registers into a 64-byte area, then calls
	// goJNIEntry(id, &save) so one dispatcher can serve every JNI function
	// regardless of its signature.
	//
	// The guest reaches a table entry through an indirect call, so on entry
	// [SP] is its return address (recorded for the abort path) and SP is 8
	// (mod 16). Six pushes plus the 64-byte save area leave SP 8 (mod 16); the
	// 200-byte argument area brings it back to 0 before the CALL, which is what
	// Go's ABIInternal requires.
	TEXT ·jniEntry(SB), NOSPLIT|NOFRAME, $0-0
		MOVQ 0(SP), R11  // guest return address
		MOVQ -8(FS), AX  // g
		MOVQ 0(AX), AX   // g.stack.lo == guest stack base
		MOVQ R11, -16(AX) // record it in this goroutine's capture area
		PUSHQ BX
		PUSHQ BP
		PUSHQ R12
		PUSHQ R13
		PUSHQ R14
		PUSHQ R15
		MOVQ -8(FS), R14 // load g for the Go callee
		SUBQ $64, SP
		MOVQ SP, R11 // R11 = register-save base
		MOVQ DI, 0(R11)
		MOVQ SI, 8(R11)
		MOVQ DX, 16(R11)
		MOVQ CX, 24(R11)
		MOVQ R8, 32(R11)
		MOVQ R9, 40(R11)
		MOVQ R10, AX // Go arg 1: function id
		MOVQ R11, BX // Go arg 2: register-save base
		HOSTCALL(·jniTarget(SB))
		// Get<Float|Double>Field and Call<Float|Double>Method return in
		// XMM0 under the SysV ABI; the Go dispatcher returns the value's bits
		// in AX. Mirror them into X0 for every JNI call: callers of integer
		// returns never read X0, which is caller-saved anyway.
		MOVQ AX, X0
		ADDQ $64, SP
		POPQ R15
		POPQ R14
		POPQ R13
		POPQ R12
		POPQ BP
		POPQ BX
		RET

	// func jniEntryAddr() uintptr
	// Returns the raw ABI0 body of jniEntry, the jump target for JNI table
	// trampolines (see variadicEntryAddr for why this getter exists).
	TEXT ·jniEntryAddr(SB), NOSPLIT, $0-8
		LEAQ ·jniEntry(SB), AX
		MOVQ AX, ret+0(FP)
		RET

	// func callGuestOnGuestStack(gs *guestStack, fn, a, b, c uintptr) uintptr
	//
	// Saves the Go SP into gs.goSP, switches RSP to the guest stack (gs.sp),
	// calls the guest function fn(a, b, c), then switches back to the Go stack
	// and returns fn's result. RBX is callee-saved under SysV, so the guest
	// preserves it across the call; we keep gs there. All FP-relative argument
	// loads happen before SP is changed (Go asm resolves FP* against SP).
	TEXT ·callGuestOnGuestStack(SB), NOSPLIT|NOFRAME, $0-48
		MOVQ fn+8(FP), R10
		MOVQ a+16(FP), DI
		MOVQ b+24(FP), SI
		MOVQ c+32(FP), DX
		MOVQ gs+0(FP), R9
		PUSHQ BX
		MOVQ R9, BX
		MOVQ SP, 24(BX) // gs.goSP = current Go SP
		MOVQ 16(BX), SP // switch to the guest stack
		CALL R10
		MOVQ SP, 16(BX) // gs.sp = guest SP on return
		MOVQ 24(BX), SP // restore the Go SP
		POPQ BX
		MOVQ AX, ret+40(FP)
		RET

	// func callGuestDirect(fn, a, b, c uintptr) uintptr
	//
	// Calls a guest function without changing stack, for re-entrant callbacks
	// while guest code (and therefore the guest stack) is already active.
	TEXT ·callGuestDirect(SB), NOSPLIT|NOFRAME, $0-40
		MOVQ fn+0(FP), R10
		MOVQ a+8(FP), DI
		MOVQ b+16(FP), SI
		MOVQ c+24(FP), DX
		PUSHQ BP
		MOVQ SP, BP
		ANDQ $-16, SP
		CALL R10
		MOVQ BP, SP
		POPQ BP
		MOVQ AX, ret+32(FP)
		RET

	// func callGuestOnGuestStack8(gs *guestStack, fn, a, b, c, d, e, f, g, h uintptr) uintptr
	//
	// callGuestOnGuestStack for a guest function whose eight integer arguments
	// do not all fit in the SysV argument registers. a..f travel in
	// DI,SI,DX,CX,R8,R9 and g,h are pushed on the guest stack, where the callee
	// finds them at [rsp+8] and [rsp+16] as the ABI requires. This is what lets
	// the runtime drive a JNI native method with six Java parameters (env, thiz
	// plus six), such as GameActivity.initializeNativeCode.
	//
	// As in callGuestOnGuestStack, BX (callee-saved) holds gs across the call
	// and every FP-relative load happens before SP is repointed.
	TEXT ·callGuestOnGuestStack8(SB), NOSPLIT|NOFRAME, $0-88
		MOVQ gs+0(FP), R11
		PUSHQ BX
		MOVQ R11, BX
		MOVQ fn+8(FP), R10
		MOVQ a+16(FP), DI
		MOVQ b+24(FP), SI
		MOVQ c+32(FP), DX
		MOVQ d+40(FP), CX
		MOVQ e+48(FP), R8
		MOVQ f+56(FP), R9
		MOVQ a7+64(FP), R11 // arg 7
		MOVQ a8+72(FP), AX  // arg 8
		MOVQ SP, 24(BX)    // gs.goSP = current Go SP
		MOVQ 16(BX), SP    // switch to the guest stack
		SUBQ $16, SP
		MOVQ R11, 0(SP) // arg 7 lands at the callee's [rsp+8]
		MOVQ AX, 8(SP)  // arg 8 lands at the callee's [rsp+16]
		CALL R10
		ADDQ $16, SP
		MOVQ SP, 16(BX) // gs.sp = guest SP on return
		MOVQ 24(BX), SP // restore the Go SP
		POPQ BX
		MOVQ AX, ret+80(FP)
		RET

	// func callGuestDirect8(fn, a, b, c, d, e, f, g, h uintptr) uintptr
	//
	// callGuestOnGuestStack8's re-entrant twin: the same eight-argument
	// convention without switching stacks.
	TEXT ·callGuestDirect8(SB), NOSPLIT|NOFRAME, $0-80
		MOVQ fn+0(FP), R10
		MOVQ a7+56(FP), R11
		MOVQ a8+64(FP), AX
		PUSHQ BP
		MOVQ SP, BP
		ANDQ $-16, SP
		SUBQ $16, SP
		MOVQ R11, 0(SP)
		MOVQ AX, 8(SP)
		MOVQ a+8(FP), DI
		MOVQ b+16(FP), SI
		MOVQ c+24(FP), DX
		MOVQ d+32(FP), CX
		MOVQ e+40(FP), R8
		MOVQ f+48(FP), R9
		CALL R10
		MOVQ BP, SP
		POPQ BP
		MOVQ AX, ret+72(FP)
		RET

	// func callCOnGuestStack(gs *guestStack, fn uintptr, args *[9]uintptr) uintptr
	//
	// Calls a *host* C function (libX11, libEGL, libGLESv2 — anything reachable
	// through the host dynamic linker) with up to nine integer arguments, on the
	// guest stack. It is the mirror image of callGuestOnGuestStack: same SP
	// switch and same baseline convention, but it loads the argument vector from
	// args[] so it can serve functions with more than six parameters, which the
	// register-only guest-call helpers cannot. GPU and window-system entry points
	// need the full SysV argument list, and the guest stack (32 MiB) is where a
	// driver's deep initialization can safely run.
	//
	// args[0..5] travel in DI,SI,DX,CX,R8,R9; args[6..8] are pushed. BX is
	// callee-saved under SysV and holds gs across the call.
	TEXT ·callCOnGuestStack(SB), NOSPLIT|NOFRAME, $0-32
		MOVQ gs+0(FP), R9
		MOVQ fn+8(FP), R10
		MOVQ args+16(FP), R11
		PUSHQ BX
		MOVQ R9, BX
		MOVQ SP, 24(BX) // gs.goSP = current Go SP
		MOVQ 16(BX), SP // switch to the guest stack
		SUBQ $32, SP
		MOVQ 48(R11), AX // args[6] -> callee [rsp+8]
		MOVQ AX, 0(SP)
		MOVQ 56(R11), AX // args[7] -> callee [rsp+16]
		MOVQ AX, 8(SP)
		MOVQ 64(R11), AX // args[8] -> callee [rsp+24]
		MOVQ AX, 16(SP)
		MOVQ 0(R11), DI
		MOVQ 8(R11), SI
		MOVQ 16(R11), DX
		MOVQ 24(R11), CX
		MOVQ 32(R11), R8
		MOVQ 40(R11), R9
		CALL R10
		ADDQ $32, SP
		MOVQ SP, 16(BX) // gs.sp = guest SP on return
		MOVQ 24(BX), SP // restore the Go SP
		POPQ BX
		MOVQ -8(FS), R14 // restore g before returning to Go
		MOVQ AX, ret+24(FP)
		RET

	// func callCDirect(fn uintptr, args *[9]uintptr) uintptr
	//
	// callCOnGuestStack's re-entrant twin: calls a host C function with the same
	// nine-argument convention without switching stacks, for when guest code (and
	// therefore the guest stack) is already active.
	TEXT ·callCDirect(SB), NOSPLIT|NOFRAME, $0-24
		MOVQ fn+0(FP), R10
		MOVQ args+8(FP), R11
		PUSHQ BP
		MOVQ SP, BP
		ANDQ $-16, SP
		SUBQ $32, SP
		MOVQ 48(R11), AX
		MOVQ AX, 0(SP)
		MOVQ 56(R11), AX
		MOVQ AX, 8(SP)
		MOVQ 64(R11), AX
		MOVQ AX, 16(SP)
		MOVQ 0(R11), DI
		MOVQ 8(R11), SI
		MOVQ 16(R11), DX
		MOVQ 24(R11), CX
		MOVQ 32(R11), R8
		MOVQ 40(R11), R9
		CALL R10
		MOVQ BP, SP
		POPQ BP
		MOVQ AX, ret+16(FP)
		RET


// func callGuestOnGuestStackWide(gs *guestStack, fn uintptr, args *[guestWideArgs]uintptr, fl *[4]uint64) uintptr
//
// Calls a guest function with up to guestWideArgs (18) integer arguments and
// four float arguments, on the guest stack. Integer arguments 1-6 go in DI, SI,
// DX, CX, R8, R9 and the rest on the stack (12 slots are always written; the
// callee ignores the unused ones). fl[0..3] are loaded into X0..X3 as raw
// bits, so a float32 argument is passed as its bits in the low half.
TEXT ·callGuestOnGuestStackWide(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ gs+0(FP), R11
	PUSHQ BX
	MOVQ R11, BX
	MOVQ fn+8(FP), R10
	MOVQ args+16(FP), R12
	MOVQ fl+24(FP), R13
	MOVQ SP, 24(BX) // gs.goSP = current Go SP
	MOVQ 16(BX), SP // switch to the guest stack
	SUBQ $96, SP
	MOVQ 48(R12), AX
	MOVQ AX, 0(SP)
	MOVQ 56(R12), AX
	MOVQ AX, 8(SP)
	MOVQ 64(R12), AX
	MOVQ AX, 16(SP)
	MOVQ 72(R12), AX
	MOVQ AX, 24(SP)
	MOVQ 80(R12), AX
	MOVQ AX, 32(SP)
	MOVQ 88(R12), AX
	MOVQ AX, 40(SP)
	MOVQ 96(R12), AX
	MOVQ AX, 48(SP)
	MOVQ 104(R12), AX
	MOVQ AX, 56(SP)
	MOVQ 112(R12), AX
	MOVQ AX, 64(SP)
	MOVQ 120(R12), AX
	MOVQ AX, 72(SP)
	MOVQ 128(R12), AX
	MOVQ AX, 80(SP)
	MOVQ 136(R12), AX
	MOVQ AX, 88(SP)
	MOVQ 0(R13), X0
	MOVQ 8(R13), X1
	MOVQ 16(R13), X2
	MOVQ 24(R13), X3
	MOVQ 0(R12), DI
	MOVQ 8(R12), SI
	MOVQ 16(R12), DX
	MOVQ 24(R12), CX
	MOVQ 32(R12), R8
	MOVQ 40(R12), R9
	MOVL $4, AX // vector registers used, for a variadic callee
	CALL R10
	ADDQ $96, SP
	MOVQ SP, 16(BX) // gs.sp = guest SP on return
	MOVQ 24(BX), SP // restore the Go SP
	POPQ BX
	MOVQ -8(FS), R14 // restore g before returning to Go
	MOVQ AX, ret+32(FP)
	RET
