#include "textflag.h"

// rawSysNoEnter is deliberately a leaf syscall: Go's syscall.Syscall performs
// entersyscall, which is invalid when called from a guest callback running on
// the guest stack.
TEXT ·rawSysNoEnter(SB),NOSPLIT,$0-40
	MOVQ trap+0(FP), AX
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	SYSCALL
	MOVQ AX, ret+32(FP)
	RET

TEXT ·rawSysNoEnter6(SB),NOSPLIT,$0-64
	MOVQ trap+0(FP), AX
	MOVQ a+8(FP), DI
	MOVQ b+16(FP), SI
	MOVQ c+24(FP), DX
	MOVQ d+32(FP), R10
	MOVQ e+40(FP), R8
	MOVQ f+48(FP), R9
	SYSCALL
	MOVQ AX, ret+56(FP)
	RET
