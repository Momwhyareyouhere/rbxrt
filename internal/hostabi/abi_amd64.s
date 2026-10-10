#include "textflag.h"

// func callGuestVoid(fn uintptr)
//
// Invokes a guest function that takes no arguments and returns nothing (a
// DT_INIT / DT_INIT_ARRAY entry point). The guest uses the SysV ABI, which
// requires %rsp to be 16-byte aligned at the call site; our own entry has
// %rsp == 8 (mod 16) because our Go caller aligned before CALL, so we subtract
// 8 first.
TEXT ·callGuestVoid(SB), NOSPLIT, $0-8
	MOVQ fn+0(FP), AX
	SUBQ $8, SP
	CALL AX
	ADDQ $8, SP
	RET

// func callGuest0(fn uintptr) uintptr
//
// Invokes a guest function with no arguments and returns its integer result.
// Used for IFUNC/IRELATIVE resolvers, which return the resolved address.
TEXT ·callGuest0(SB), NOSPLIT, $0-16
	MOVQ fn+0(FP), AX
	SUBQ $8, SP
	CALL AX
	ADDQ $8, SP
	MOVQ AX, ret+8(FP)
	RET

// func callGuestInt3(fn, a, b, c uintptr) int
//
// Invokes a guest function with three integer arguments and returns its result.
// Used for callbacks such as dl_iterate_phdr's visitor and bsearch comparators.
TEXT ·callGuestInt3(SB), NOSPLIT, $0-40
	MOVQ fn+0(FP), R10
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	SUBQ $8, SP
	CALL R10
	ADDQ $8, SP
	MOVQ AX, ret+32(FP)
	RET

// func callGuestVoid1(fn, a uintptr)
//
// Invokes a guest function with one integer argument and no return value. Used
// for atexit-style destructors: void (*)(void*).
TEXT ·callGuestVoid1(SB), NOSPLIT, $0-16
	MOVQ fn+0(FP), AX
	MOVQ a+8(FP), DI
	SUBQ $8, SP
	CALL AX
	ADDQ $8, SP
	RET

// func callHostRaw(fn, a, b, c uintptr) uintptr
//
// Calls a host function through its thunk as guest code would (SysV argument
// registers). It exists so the runtime can self-test the host library and is
// also used by the loader's symbol-resolution checks.
TEXT ·callHostRaw(SB), NOSPLIT, $0-40
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
