package hostabi

import (
	"math"
	"testing"
	"unsafe"
)

// TestCallGuestWide checks that the 18th integer argument lands on the stack
// where the SysV callee looks for it, and that floats reach X1 and X3.
func TestCallGuestWide(t *testing.T) {
	code := [][]byte{
		{0x48, 0x8B, 0x44, 0x24, 0x60, 0xC3}, // MOVQ 96(SP), AX ; RET  (integer arg 18)
		{0x66, 0x0F, 0x7E, 0xC8, 0xC3},       // MOVD X1, AX ; RET     (float arg 2)
		{0x48, 0x89, 0xC8, 0xC3},             // MOVQ CX, AX ; RET     (integer arg 4)
		{0x66, 0x0F, 0x7E, 0xD8, 0xC3},       // MOVD X3, AX ; RET     (float arg 4)
	}
	var fns []uintptr
	for _, c := range code {
		b := codeArena(len(c))
		copy(b, c)
		fns = append(fns, uintptr(unsafe.Pointer(&b[0])))
	}
	arenaMakeExec()

	ints := make([]uintptr, guestWideArgs)
	for i := range ints {
		ints[i] = uintptr(1000 + i)
	}
	if r := CallGuestWide(fns[0], ints, 1.5, 2.5); r != 1017 {
		t.Fatalf("integer arg 18 = %d, want 1017", r)
	}
	if r := CallGuestWide(fns[1], ints, 1.5, 2.5); math.Float32frombits(uint32(r)) != 2.5 {
		t.Fatalf("float arg 2 = %v, want 2.5", math.Float32frombits(uint32(r)))
	}
	if r := CallGuestWide(fns[2], ints); r != 1003 {
		t.Fatalf("integer arg 4 = %d, want 1003", r)
	}
	if r := CallGuestWide(fns[3], nil, 1, 2, 3, 4.5); math.Float32frombits(uint32(r)) != 4.5 {
		t.Fatalf("float arg 4 = %v, want 4.5", math.Float32frombits(uint32(r)))
	}
}
