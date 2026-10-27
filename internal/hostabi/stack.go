package hostabi

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Guest code runs on a dedicated, mmap'd stack rather than the Go goroutine
// stack. Running the guest on the Go stack means the Go runtime's stack-growth
// machinery (and GC stack scan) eventually walks a guest frame it has no
// metadata for, aborting with "unknown caller pc". A separate guest stack keeps
// Go frames and guest frames on different stacks entirely.
//
// While guest code runs, every host function it calls still executes Go code,
// but on the *guest* stack (thunks call their Go target directly). Two things
// make that safe:
//
//   - The goroutine's stack guards are zeroed for the duration (see
//     setGuardsZeroed), so a host function's prologue never decides it must grow
//     the (unrelated) Go stack, which would trigger a copy.
//   - The GC is inhibited globally (see the hostabi init), so the Go runtime
//     never scans the goroutine's stack region while guest frames are live.
//
// A host function that itself calls back into the guest (a callback) re-enters
// through CallGuest*, which detects it is already on the guest stack (depth > 0)
// and calls the guest directly rather than switching again.
const (
	// guestStackSize bounds a single guest thread's stack. It is deliberately
	// generous: the engine recurses through its task machinery and a renderer's
	// frames are large. The region is mapped with MAP_NORESERVE, so only the
	// pages actually touched are backed.
	guestStackSize = 256 << 20
	// guestGuardSize is extra address space below the usable region, so an
	// overflow lands in space adjacent to the mapping rather than inside it.
	guestGuardSize = 4 << 20

	// Every host thunk records the guest return address, frame pointer and SP
	// of its call site in the guest stack's guard region, three words below the
	// stack's low bound (base). invokeGuest points g.stack.lo at base for the
	// whole time guest code runs, so a thunk finds the slot from g alone; see
	// mkThunk. Per-goroutine storage is what lets a fatal handler name the real
	// caller while other guest threads are running.
	guestCaptureRetPC  = 16 // stored at base - 16
	guestCaptureRBP    = 8  // stored at base - 8
	guestCaptureRSP    = 24 // stored at base - 24
	guestCaptureTLSKey = ^uint32(0)
)

// guestStack describes one goroutine's guest stack. The field order and offsets
// are relied on by assembly (see callGuestOnGuestStack in abi_amd64.s):
//
//	base   +0
//	top    +8
//	sp     +16  entry SP for the next top-level guest call
//	goSP   +24  saved Go SP while guest code runs
//	depth  +32
//
// Fields after depth are host-only bookkeeping and must stay at the end.
type guestStack struct {
	base  uintptr
	top   uintptr
	sp    uintptr
	goSP  uintptr
	depth int

	mem     uintptr // start of the mapping, for munmap
	memSize int
}

var (
	gsMu  sync.Mutex
	gsMap = map[uintptr]*guestStack{}

	debugOnce sync.Once
)

func allocGuestStack() *guestStack {
	size := guestStackSize + guestGuardSize
	b, err := syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		panic("hostabi: cannot allocate guest stack: " + err.Error())
	}
	mem := uintptr(unsafe.Pointer(&b[0]))
	base := mem + guestGuardSize
	top := base + guestStackSize
	return &guestStack{base: base, top: top, sp: top &^ 15, mem: mem, memSize: size}
}

// release unmaps the guest stack. It is called once a guest thread's goroutine
// has finished with it, so that a recycled goroutine does not inherit the
// previous thread's stack (and, with it, stale guest frames).
func (gs *guestStack) release() {
	if gs == nil || gs.mem == 0 {
		return
	}
	mem, size := gs.mem, gs.memSize
	gs.mem, gs.memSize = 0, 0
	_ = syscall.Munmap(unsafe.Slice((*byte)(unsafe.Pointer(mem)), size))
}

func getGuestStack(g uintptr) *guestStack {
	gsMu.Lock()
	defer gsMu.Unlock()
	gs := gsMap[g]
	if gs == nil {
		gs = allocGuestStack()
		gsMap[g] = gs
	}
	return gs
}

// EnsureGuestStack allocates the current goroutine's guest stack if needed.
func EnsureGuestStack() { _ = getGuestStack(getg()) }

// guestCapture returns the guest PC and frame pointer recorded by the most
// recent host thunk on the calling goroutine, or zeros if it has never entered
// guest code. It does not allocate, so it is safe to call from a fatal path.
func guestCapture() (retPC, rbp uintptr) {
	g := getg()
	gsMu.Lock()
	gs := gsMap[g]
	gsMu.Unlock()
	if gs == nil || gs.base == 0 {
		return 0, 0
	}
	return *(*uintptr)(unsafe.Pointer(gs.base - guestCaptureRetPC)),
		*(*uintptr)(unsafe.Pointer(gs.base - guestCaptureRBP))
}

// GuestThreadState describes one guest thread (one goroutine with a guest
// stack) at the moment it was sampled. The PC is the guest address of the most
// recent host call that thread made: for a thread blocked inside a host
// function it names exactly where the guest is, and for a thread running pure
// guest code it is the last call site, i.e. a sample of its recent activity.
type GuestThreadState struct { //nolint:revive // exported for cmd/rbxrt
	Base     uintptr
	RetPC    uintptr
	RBP      uintptr
	Depth    int
	StackTop uintptr
}

// GuestFrameChain walks a sampled guest frame pointer, stopping at the guest
// stack bounds or the first non-monotonic link. It is intended for watchdogs;
// frame-pointer omission can make the resulting chain incomplete.
func GuestFrameChain(rbp, stackTop uintptr, max int) []uintptr {
	var out []uintptr
	for i := 0; i < max && rbp != 0; i++ {
		if rbp < stackTop-guestStackSize || rbp > stackTop-16 || rbp&7 != 0 {
			break
		}
		next := *(*uintptr)(unsafe.Pointer(rbp))
		out = append(out, *(*uintptr)(unsafe.Pointer(rbp + 8)))
		if next <= rbp || next > stackTop-16 {
			break
		}
		rbp = next
	}
	return out
}

// GuestImageOffset maps a guest address in the primary image to an image
// offset, so a sampled PC can be read against the loaded file.
func GuestImageOffset(pc uintptr) uintptr {
	if guestBias != 0 && pc >= guestBias {
		return pc - guestBias
	}
	return pc
}

// GuestThreadsSnapshot samples every goroutine that owns a guest stack. It is
// read-only and takes the same lock as the capture accessors, so it is safe to
// call from another goroutine while guest threads run.
func GuestThreadsSnapshot() []GuestThreadState {
	gsMu.Lock()
	stacks := make([]*guestStack, 0, len(gsMap))
	for _, gs := range gsMap {
		stacks = append(stacks, gs)
	}
	gsMu.Unlock()
	out := make([]GuestThreadState, 0, len(stacks))
	for _, gs := range stacks {
		if gs.base == 0 {
			continue
		}
		out = append(out, GuestThreadState{
			Base:     gs.base,
			RetPC:    *(*uintptr)(unsafe.Pointer(gs.base - guestCaptureRetPC)),
			RBP:      *(*uintptr)(unsafe.Pointer(gs.base - guestCaptureRBP)),
			Depth:    gs.depth,
			StackTop: gs.top,
		})
	}
	return out
}

// guestCapturedRSP returns the guest stack pointer at the most recent host
// thunk entry, or 0 if that thunk was never entered from guest code. Callers
// that need a C stack argument (bionic's syscall() is variadic, so futex's
// bitset is passed on the stack rather than in a register) can then read the
// argument at SP+8, just above the return address. The bounds check keeps a
// stale slot from being dereferenced when the caller was not a thunk.
//
//go:nosplit
func guestCapturedRSP() uintptr {
	g := getg()
	base := *(*uintptr)(unsafe.Pointer(g))
	top := *(*uintptr)(unsafe.Pointer(g + 8))
	if base == 0 || top <= base {
		return 0
	}
	sp := *(*uintptr)(unsafe.Pointer(base - guestCaptureRSP))
	if sp < base || sp+8 > top {
		return 0
	}
	return sp
}

// withGuestStackContext points g.stack at the current goroutine's guest region
// for the duration of fn, the same way invokeGuest does while guest code runs.
//
// Every host thunk records its caller's return address and frame pointer just
// below g.stack.lo (see mkThunk), which is only inside mapped memory while
// g.stack.lo is a guest stack's base: the guest mapping keeps a guard region
// below it. A thunk entered from ordinary Go code would instead write below the
// Go stack. The only such entries are CallHostRaw and the thunk probes, so they
// wrap themselves in this to stay safe. The guards are left alone: fn runs on
// the Go stack, whose current SP is well above the (unchanged) stackguard, so no
// prologue tries to grow the stack.
func withGuestStackContext(fn func() uintptr) uintptr {
	g := getg()
	gs := getGuestStack(g)
	oldLo := *(*uintptr)(unsafe.Pointer(g))
	oldHi := *(*uintptr)(unsafe.Pointer(g + 8))
	*(*uintptr)(unsafe.Pointer(g)) = gs.base
	*(*uintptr)(unsafe.Pointer(g + 8)) = gs.top
	r := fn()
	*(*uintptr)(unsafe.Pointer(g)) = oldLo
	*(*uintptr)(unsafe.Pointer(g + 8)) = oldHi
	return r
}

// --- stack guard manipulation -------------------------------------------

// stackguardOff is the byte offset of g.stackguard0, discovered at init by
// locating the field that holds g.stack.lo+stackGuard. Deriving it avoids
// hard-coding a runtime struct layout.
var stackguardOff uintptr

const gStackGuard = 928 // runtime.stackGuard on amd64

func init() {
	g := getg()
	lo := *(*uintptr)(unsafe.Pointer(g))
	for off := uintptr(8); off < 256; off += 8 {
		if *(*uintptr)(unsafe.Pointer(g + off)) == lo+gStackGuard {
			stackguardOff = off
			return
		}
	}
	panic("hostabi: could not locate g.stackguard0")
}

// debugGuards reports the discovered guard offset and the first few g words, for
// diagnosing a mis-detected offset.
func debugGuards() {
	g := getg()
	lo := *(*uintptr)(unsafe.Pointer(g))
	hi := *(*uintptr)(unsafe.Pointer(g + 8))
	fmt.Fprintf(os.Stderr, "rbxrt: g=%#x stack=[%#x,%#x) guardOff=%d guard0=%#x (lo+%d=%#x)\n",
		g, lo, hi, stackguardOff, *(*uintptr)(unsafe.Pointer(g + stackguardOff)), gStackGuard, lo+gStackGuard)
}

// zeroGuards sets stackguard0/stackguard1 of g to 0 and returns the old values.
// A zero guard disables both stack growth and preemption checks in function
// prologues: SP is never <= 0, so morestack is never entered while the guard is
// zeroed.
func zeroGuards(g uintptr) (o0, o1 uintptr) {
	p0 := (*uintptr)(unsafe.Pointer(g + stackguardOff))
	p1 := (*uintptr)(unsafe.Pointer(g + stackguardOff + 8))
	o0, o1 = *p0, *p1
	*p0 = 0
	*p1 = 0
	return
}

func restoreGuards(g uintptr, o0, o1 uintptr) {
	*(*uintptr)(unsafe.Pointer(g + stackguardOff)) = o0
	*(*uintptr)(unsafe.Pointer(g + stackguardOff + 8)) = o1
}

// callGuestOnGuestStack is implemented in abi_amd64.s. It saves the Go SP,
// switches to the guest stack, calls fn with (a, b, c), then switches back and
// returns fn's result.
func callGuestOnGuestStack(gs *guestStack, fn, a, b, c uintptr) uintptr

// callGuestDirect is implemented in abi_amd64.s. It calls a guest function
// without changing stacks; used for re-entrant callbacks when already on the
// guest stack.
func callGuestDirect(fn, a, b, c uintptr) uintptr

// callGuestOnGuestStack8 is implemented in abi_amd64.s. Like
// callGuestOnGuestStack, but for a guest function with eight integer arguments:
// the first six go in registers and the last two (a7, a8) on the guest stack.
// The last two are named a7/a8 because "g" is a reserved register name in the
// assembler.
func callGuestOnGuestStack8(gs *guestStack, fn, a, b, c, d, e, f, a7, a8 uintptr) uintptr

// callGuestDirect8 is implemented in abi_amd64.s. It is the re-entrant twin of
// callGuestOnGuestStack8 (no stack switch).
func callGuestDirect8(fn, a, b, c, d, e, f, a7, a8 uintptr) uintptr

// invokeGuest is the single host->guest entry used by all public CallGuest*.
func invokeGuest(fn, a, b, c uintptr) uintptr {
	g := getg()
	gs := getGuestStack(g)
	if debugStack {
		lo := *(*uintptr)(unsafe.Pointer(g))
		hi := *(*uintptr)(unsafe.Pointer(g + 8))
		sp := currentSP()
		fmt.Fprintf(os.Stderr, "invokeGuest fn=%#x g=%#x depth=%d gs=[%#x,%#x) gs.sp=%#x gstack=[%#x,%#x) sp=%#x onGuest=%v\n",
			fn, g, gs.depth, gs.base, gs.top, gs.sp, lo, hi, sp, sp >= gs.base && sp < gs.top)
	}
	if gs.depth > 0 {
		return callGuestDirect(fn, a, b, c)
	}
	gs.depth = 1
	// Start every top-level guest call from the top of the guest stack. The
	// guest (and any host Go code it calls) is expected to leave SP balanced,
	// but resetting makes the invariant explicit and keeps a stray unbalanced
	// return from eating the stack across many calls.
	gs.sp = gs.top &^ 15
	// While guest code runs, its host calls execute Go on the guest stack.
	// Make the runtime accept that stack: point g.stack at the guest region
	// (syscalls, preemption and scheduling all validate SP against g.stack) and
	// zero the guards so no prologue tries to grow or copy it.
	oldLo := *(*uintptr)(unsafe.Pointer(g))
	oldHi := *(*uintptr)(unsafe.Pointer(g + 8))
	debugOnce.Do(func() {
		if !debugMem {
			return
		}
		debugGuards()
		fmt.Fprintf(os.Stderr, "rbxrt: gs.base=%#x gs.top=%#x span=%#x\n", gs.base, gs.top, gs.top-gs.base)
	})
	*(*uintptr)(unsafe.Pointer(g)) = gs.base
	*(*uintptr)(unsafe.Pointer(g + 8)) = gs.top
	o0, o1 := zeroGuards(g)
	if debugMem {
		fmt.Fprintf(os.Stderr, "invokeGuest fn=%#x entrySP=%#x top=%#x base=%#x\n", fn, gs.sp, gs.top, gs.base)
	}
	r := callGuestOnGuestStack(gs, fn, a, b, c)
	restoreGuards(g, o0, o1)
	*(*uintptr)(unsafe.Pointer(g)) = oldLo
	*(*uintptr)(unsafe.Pointer(g + 8)) = oldHi
	gs.depth = 0
	return r
}

// invokeGuest8 is invokeGuest for a guest function with eight integer
// arguments. The first six travel in registers and the last two on the stack
// (see callGuestOnGuestStack8); everything else — guest-stack switch, stack
// guard suppression, depth handling — is identical, so the two must be kept in
// step.
func invokeGuest8(fn, a, b, c, d, e, f, g, h uintptr) uintptr {
	gp := getg()
	gs := getGuestStack(gp)
	if gs.depth > 0 {
		return callGuestDirect8(fn, a, b, c, d, e, f, g, h)
	}
	gs.depth = 1
	gs.sp = gs.top &^ 15
	oldLo := *(*uintptr)(unsafe.Pointer(gp))
	oldHi := *(*uintptr)(unsafe.Pointer(gp + 8))
	*(*uintptr)(unsafe.Pointer(gp)) = gs.base
	*(*uintptr)(unsafe.Pointer(gp + 8)) = gs.top
	o0, o1 := zeroGuards(gp)
	if debugMem {
		fmt.Fprintf(os.Stderr, "invokeGuest8 fn=%#x entrySP=%#x top=%#x base=%#x\n", fn, gs.sp, gs.top, gs.base)
	}
	r := callGuestOnGuestStack8(gs, fn, a, b, c, d, e, f, g, h)
	restoreGuards(gp, o0, o1)
	*(*uintptr)(unsafe.Pointer(gp)) = oldLo
	*(*uintptr)(unsafe.Pointer(gp + 8)) = oldHi
	gs.depth = 0
	return r
}

// DebugGuestDepth reports the current guest depth for the calling goroutine.
func DebugGuestDepth() int { return getGuestStack(getg()).depth }

// currentSP returns the caller's stack pointer (implemented in abi_amd64.s).
//
//go:noescape
func currentSP() uintptr

// spProbe tracks the lowest guest-stack address seen by host code, for
// diagnosing runaway guest-stack consumption.
var spProbe struct {
	mu  sync.Mutex
	min uintptr
}

func noteSP(tag string) {
	if !debugMem {
		return
	}
	sp := currentSP()
	gs := getGuestStack(getg())
	spProbe.mu.Lock()
	if spProbe.min == 0 || sp+8<<20 < spProbe.min {
		spProbe.min = sp
		fmt.Fprintf(os.Stderr, "rbxrt: SP %s %#x usage=%#x depth=%d\n", tag, sp, gs.top-sp, gs.depth)
	}
	spProbe.mu.Unlock()
}
