package hostabi

import (
	"math"
	"unsafe"
)

// guestWideArgs is the most integer arguments CallGuestWide passes.
const guestWideArgs = 18

// callGuestOnGuestStackWide is implemented in abi_amd64.s.
//
//go:noescape
func callGuestOnGuestStackWide(gs *guestStack, fn uintptr, args *[guestWideArgs]uintptr, fl *[4]uint64) uintptr

// CallGuestWide calls a guest function taking up to 18 integer arguments and
// up to four float32 arguments (each class in declaration order), such as the
// engine's nativePassMouseMove(env, cls, x, y, dx, dy). It must be called from
// the top level (not from inside a host function the guest called), like the
// launch driver's other lifecycle calls.
func CallGuestWide(fn uintptr, ints []uintptr, floats ...float32) uintptr {
	if len(ints) > guestWideArgs || len(floats) > 4 {
		panic("hostabi: CallGuestWide: too many arguments")
	}
	var args [guestWideArgs]uintptr
	copy(args[:], ints)
	var fl [4]uint64
	for i, f := range floats {
		fl[i] = uint64(math.Float32bits(f))
	}

	gp := getg()
	gs := getGuestStack(gp)
	if gs.depth > 0 {
		panic("hostabi: CallGuestWide called while guest code is active")
	}
	gs.depth = 1
	gs.sp = gs.top &^ 15
	oldLo := *(*uintptr)(unsafe.Pointer(gp))
	oldHi := *(*uintptr)(unsafe.Pointer(gp + 8))
	*(*uintptr)(unsafe.Pointer(gp)) = gs.base
	*(*uintptr)(unsafe.Pointer(gp + 8)) = gs.top
	o0, o1 := zeroGuards(gp)
	r := callGuestOnGuestStackWide(gs, fn, &args, &fl)
	restoreGuards(gp, o0, o1)
	*(*uintptr)(unsafe.Pointer(gp)) = oldLo
	*(*uintptr)(unsafe.Pointer(gp + 8)) = oldHi
	gs.depth = 0
	return r
}
