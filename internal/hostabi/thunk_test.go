package hostabi

import (
	"runtime"
	"testing"
)

// TestThunkPreservesCalleeSaved is the regression test for the ABI bug that let
// host calls corrupt guest state: the SysV ABI (which the guest is compiled
// against) requires RBX, RBP, R12, R13, R14 and R15 to survive a call, but the
// trampoline writes R14 (to hold g) and RBX (Go's second argument register) and
// Go's ABIInternal does not keep the rest. The probe runs a hostile target that
// wipes all of them; a correct thunk restores the probe's sentinels.
func TestThunkPreservesCalleeSaved(t *testing.T) {
	th := mkThunk(funcAddr(clobberCalleeSaved))
	arenaMakeExec()
	// Run under a guest-stack context: the thunk records its capture just below
	// g.stack.lo (see mkThunk), which must be a guest stack's base.
	if mask := withGuestStackContext(func() uintptr { return thunkPreserveProbe(th) }); mask != 0 {
		t.Fatalf("host thunk did not preserve SysV callee-saved registers: mask=%#x "+
			"(bit0=RBX bit1=RBP bit2=R12 bit3=R13 bit4=R14 bit5=R15)", mask)
	}
}

var tmpGrowSink byte

//go:noinline
func tmpPreGrow(n int) {
	var pad [16384]byte
	pad[0] = tmpGrowSink
	if n > 0 {
		tmpPreGrow(n - 1)
	}
	tmpGrowSink = pad[n&0]
	runtime.KeepAlive(&pad)
}

// TestThunkReservesArgumentArea is the regression test for the bug that first
// broke the engine constructors: the thunk reserved only 8 bytes below its
// saved registers, but Go's ABIInternal requires the caller to provide an
// argument area at the top of its frame, into which the callee spills its
// incoming register arguments. A callee with two or more arguments (such as
// clock_gettime) then overwrote the thunk's saved R15 -- which happened to hold
// the guest's stack-guard pointer, producing a bogus "stack smashing" abort.
//
// The target is a real two-argument Go function, so the second spill lands
// exactly where the bug was. The stack is grown first because a Go stack walk
// that reaches the runtime-generated thunk frame is a separate, still-open
// limitation (see the dedicated guest-stack design).
func TestThunkReservesArgumentArea(t *testing.T) {
	tmpPreGrow(3000) // ~48 MiB
	th := mkThunk(funcAddr(goClockGettime))
	arenaMakeExec()
	// Run under a guest-stack context; see TestThunkPreservesCalleeSaved.
	if mask := withGuestStackContext(func() uintptr { return thunkPreserveProbe(th) }); mask != 0 {
		t.Fatalf("host thunk did not preserve callee-saved registers around a "+
			"two-argument Go target: mask=%#x "+
			"(bit0=RBX bit1=RBP bit2=R12 bit3=R13 bit4=R14 bit5=R15)", mask)
	}
}
