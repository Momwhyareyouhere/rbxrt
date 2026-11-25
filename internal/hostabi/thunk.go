package hostabi

import (
	"reflect"
	"sync"
	"syscall"
	"unsafe"
)

// The runtime exposes host functions to guest code by generating tiny
// trampolines ("thunks") in an executable memory arena. Guest code always
// reaches a host function through a PLT/GOT slot, so the linker only needs a
// single stable address per symbol.
//
// A thunk for a normal host function is:
//
//	MOVQ 0(SP), R10                 ; guest return address (diagnostic: see below)
//	MOVQ FS:-8, R11                 ; g
//	MOVQ 0(R11), R11                ; g.stack.lo == guest stack base
//	MOVQ R10, -16(R11)              ; record return address, per goroutine
//	MOVQ RBP, R10
//	MOVQ R10, -8(R11)               ; record frame pointer, per goroutine
//	MOVQ RSP, R10
//	MOVQ R10, -24(R11)              ; record caller SP, per goroutine
//	PUSHQ RBX/RBP/R12/R13/R14/R15   ; save the SysV callee-saved registers
//	MOVQ FS:-8, R14                 ; load the Go g register (ABIInternal)
//	MOVQ DI, AX                     ; SysV integer arg 1 -> Go arg 1
//	MOVQ SI, BX                     ; 2 -> 2
//	MOVQ CX, DI                     ; 4 -> 4  (read before CX is overwritten)
//	MOVQ DX, CX                     ; 3 -> 3
//	MOVQ R8, SI                     ; 5 -> 5
//	MOVQ R9, R8                     ; 6 -> 6
//	MOVQ $target, R11               ; code address of the Go implementation
//	SUBQ $thunkArgArea, SP          ; argument spill area + 16-byte alignment
//	CALL R11
//	ADDQ $thunkArgArea, SP
//	POPQ R15/R14/R13/R12/RBP/RBX    ; restore the guest's registers
//	RET
//
// The save/restore is not optional: the guest is compiled against the SysV ABI,
// in which RBX, RBP, R12, R13, R14 and R15 are callee-saved across a call. The
// trampoline itself writes R14 (to hold g) and RBX (as Go's second argument
// register), and Go's ABIInternal does not preserve the others, so a thunk that
// skipped this would silently corrupt any guest value kept in those registers.
//
// The prologue records the guest's return address and frame pointer, which are
// the only reliable way to recover a guest PC from inside a Go signal handler or
// abort path (see goStackChkFail): the Go frame layout is unknown to us and
// changes with the compiler. They are stored in the *current goroutine's* guest
// stack, two words below its low bound, because a single process-global is
// clobbered by any other guest thread that makes a host call concurrently.
// invokeGuest keeps g.stack.lo (offset 0 of g) pointing at the guest stack's
// low bound for the whole time guest code runs, so the thunk can derive the
// address from g alone. The stored values are uintptrs, so no write barrier is
// needed and nothing is kept alive.
//
// Floating-point arguments need no movement: the SysV and Go amd64 ABIs both
// place the first float argument in XMM0/X0 and return in XMM0/X0. Integer and
// float registers are assigned independently in both ABIs, so one thunk shape
// serves every signature with up to six integer arguments.
//
// A trap thunk is:
//
//	MOVQ FS:-8, R14
//	MOVQ $name, AX         ; pointer to the symbol name (C string)
//	MOVQ $trapGo, R11
//	SUBQ $8, SP
//	CALL R11
//	ADDQ $8, SP
//	RET
//
// trapGo never returns: it reports the missing symbol and aborts.

const (
	// thunkArgArea is the number of bytes a thunk reserves below its saved
	// registers for the Go callee to spill its register-passed arguments into.
	//
	// Go's ABIInternal requires the *caller* to provide this area at the top of
	// its frame: the callee writes each incoming register argument there (see
	// the MOVQ AX,0x28(SP) spill in a typical prologue). If the area is too
	// small, a callee with two or more arguments overwrites the callee-saved
	// registers the thunk pushed just above it. The ABI can pass up to 9
	// integer arguments and 15 floating-point arguments in registers, so 24
	// slots is the maximum; the extra 8 bytes keep the call site 16-byte
	// aligned (a thunk entry is 8 mod 16).
	thunkArgArea = 24*8 + 8 // 200

	// A host thunk is ~220 bytes of code (see thunkCallTail); 256 keeps them
	// naturally spaced in the arena.
	thunkSize    = 256
	trapThunkLen = 64
)

// emitSubAddImm emits SUBQ/ADDQ $imm32, SP. imm must fit in int32.
func emitSubAddImm(b []byte, sub bool, imm int32) {
	b[0] = 0x48
	b[1] = 0x81
	if sub {
		b[2] = 0xEC // /5: SUB
	} else {
		b[2] = 0xC4 // /0: ADD
	}
	b[3] = byte(imm)
	b[4] = byte(imm >> 8)
	b[5] = byte(imm >> 16)
	b[6] = byte(imm >> 24)
}

var goShimFSPrefix = []byte{0x64, 0x4C, 0x8B, 0x34, 0x25, 0xF8, 0xFF, 0xFF, 0xFF} // MOVQ FS:-8, R14
var movR11Prefix = []byte{0x49, 0xBB}                                             // MOVQ imm64, R11
var movAXPrefix = []byte{0x48, 0xB8}                                              // MOVQ imm64, AX

var (
	arenaMu   sync.Mutex
	arenas    [][]byte // finalized (executable) slabs, oldest first
	arena     []byte   // current writable slab, or nil
	arenaOff  int
	arenaExec bool
)

const arenaSize = 1 << 20 // 1 MiB of trampolines is far more than the ~600 we need

// codeArena returns a pointer to n writable bytes in the arena, growing the
// arena as needed. The caller writes code, then calls arenaMakeExec.
//
// A slab that has been made executable is never reopened for writing: doing so
// would leave guest-callable code non-executable for the duration, and guest
// threads may be running at any time after image load. Instead, a full (or
// already-executed) slab causes a fresh slab to be mapped, so previously
// published thunk addresses stay executable forever.
func codeArena(n int) []byte {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	needNew := arena == nil || arenaOff+n > len(arena)
	if !needNew && arenaExec {
		if guestThreads.Load() == 0 {
			// No guest code can be executing, so reopening the current slab
			// for writing is safe. Callers must arenaMakeExec again before the
			// new thunk can run.
			if err := syscall.Mprotect(arena, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
				panic("hostabi: cannot reopen code arena: " + err.Error())
			}
			arenaExec = false
		} else {
			// Guest threads are live: never render a published thunk page
			// non-executable. Map a fresh slab instead.
			needNew = true
		}
	}
	if needNew {
		m, err := syscall.Mmap(-1, 0, arenaSize,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			panic("hostabi: cannot allocate code arena: " + err.Error())
		}
		if arena != nil {
			arenas = append(arenas, arena)
		}
		arena = m
		arenaOff = 0
		arenaExec = false
	}
	b := arena[arenaOff : arenaOff+n]
	arenaOff += n
	return b
}

// arenaMakeExec flips the current writable slab to read+execute. It is
// idempotent and safe to call while another thunk is being written (the write
// lock is taken again by codeArena). Earlier slabs stay executable.
func arenaMakeExec() {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	if arena == nil || arenaExec {
		return
	}
	if err := syscall.Mprotect(arena, syscall.PROT_READ|syscall.PROT_EXEC); err != nil {
		panic("hostabi: cannot make code arena executable: " + err.Error())
	}
	arenaExec = true
}

// funcAddr returns the ABIInternal entry point of a Go function. This is the
// address that a thunk must call.
func funcAddr(fn any) uintptr {
	return reflect.ValueOf(fn).Pointer()
}

func put64(b []byte, v uintptr) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

// mkThunk writes and returns a trampoline for the Go function at target.
func mkThunk(target uintptr) uintptr {
	b := codeArena(thunkSize)
	off := 0
	// Record the guest return address and frame pointer in this goroutine's
	// guest-stack capture area (two words just below its low bound). The address
	// comes from g.stack.lo, which invokeGuest points at that bound while guest
	// code runs; see the file comment. Per-goroutine storage is what makes the
	// value trustworthy with more than one guest thread.
	copy(b[off:], []byte{0x4C, 0x8B, 0x14, 0x24}) // MOVQ 0(SP), R10
	off += 4
	copy(b[off:], []byte{0x64, 0x4C, 0x8B, 0x1C, 0x25, 0xF8, 0xFF, 0xFF, 0xFF}) // MOVQ FS:-8, R11
	off += 9
	copy(b[off:], []byte{0x4D, 0x8B, 0x1B}) // MOVQ 0(R11), R11
	off += 3
	copy(b[off:], []byte{0x4D, 0x89, 0x53, 0xF0}) // MOVQ R10, -16(R11)
	off += 4
	// Record the guest frame pointer so a diagnostic can walk the guest's RBP
	// chain. RBP is untouched before the PUSHQ RBP below.
	copy(b[off:], []byte{0x49, 0x89, 0xEA}) // MOVQ RBP, R10
	off += 3
	copy(b[off:], []byte{0x4D, 0x89, 0x53, 0xF8}) // MOVQ R10, -8(R11)
	off += 4
	// Record the guest SP too. bionic's syscall() is variadic, so a call with
	// six syscall arguments (futex needs a bitset as its sixth) leaves the last
	// one on the stack, out of reach of the register mapping below. SP still
	// points at the return address here, so that argument is at SP+8.
	copy(b[off:], []byte{0x49, 0x89, 0xE2}) // MOVQ RSP, R10
	off += 3
	copy(b[off:], []byte{0x4D, 0x89, 0x53, 0xE8}) // MOVQ R10, -24(R11)
	off += 4
	// Save the SysV callee-saved registers the guest relies on. RBX is Go's
	// second argument register and R14 is loaded with g, so both would be
	// clobbered without this prologue.
	copy(b[off:], []byte{0x53}) // PUSHQ RBX
	off++
	copy(b[off:], []byte{0x55}) // PUSHQ RBP
	off++
	copy(b[off:], []byte{0x41, 0x54}) // PUSHQ R12
	off += 2
	copy(b[off:], []byte{0x41, 0x55}) // PUSHQ R13
	off += 2
	copy(b[off:], []byte{0x41, 0x56}) // PUSHQ R14
	off += 2
	copy(b[off:], []byte{0x41, 0x57}) // PUSHQ R15
	off += 2
	copy(b[off:], goShimFSPrefix) // MOVQ FS:-8, R14
	off += len(goShimFSPrefix)
	copy(b[off:], []byte{0x48, 0x89, 0xF8}) // MOVQ DI, AX
	off += 3
	copy(b[off:], []byte{0x48, 0x89, 0xF3}) // MOVQ SI, BX
	off += 3
	copy(b[off:], []byte{0x48, 0x89, 0xCF}) // MOVQ CX, DI
	off += 3
	copy(b[off:], []byte{0x48, 0x89, 0xD1}) // MOVQ DX, CX
	off += 3
	copy(b[off:], []byte{0x4C, 0x89, 0xC6}) // MOVQ R8, SI
	off += 3
	copy(b[off:], []byte{0x4D, 0x89, 0xC8}) // MOVQ R9, R8
	off += 3
	// The seventh SysV integer argument is at 8(SP) on entry. Go's internal
	// ABI has a seventh integer argument register (R9); forward it there for
	// shims whose C signatures exceed six arguments.
	copy(b[off:], []byte{0x4C, 0x8B, 0x4C, 0x24, 0x08}) // MOVQ 8(SP), R9
	off += 5
	copy(b[off:], thunkCallTail)
	put64(b[off+thunkTailTarget1:], target)
	put64(b[off+thunkTailTarget2:], target)
	off += len(thunkCallTail)
	if off > thunkSize {
		panic("hostabi: host thunk overflowed its code buffer")
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

// thunkCallTail is the end of every host thunk: it calls the Go target and
// restores the guest's callee-saved registers. On entry R14 holds g and the
// arguments are in Go's ABI registers; R10-R13 and R15 are free (the guest's
// values were pushed).
//
// The engine runs much of its work on fibers - stacks it mmaps and switches to
// itself - so a host call can arrive with SP outside the goroutine's guest
// stack. The Go runtime validates SP against g.stack whenever it looks
// (preemption requests, syscalls, tracebacks) and aborts with "split stack
// overflow" or "entersyscall inconsistent sp" when it is elsewhere. The tail
// therefore checks SP against [g.stack.lo, g.stack.hi) and, when it is
// outside, runs the Go call on a reserved area instead: the lowest 32 MiB of
// this thread's own guest stack, which ordinary guest execution never reaches.
// The area is handed out in 4 MiB levels, top down, so a host call that calls
// back into guest code which switches fibers again nests safely; the current
// level's top lives in the guest-stack guard slot at lo-40 (0 = unused).
// Exhausting the eight levels traps (int3) rather than overlapping a live
// frame. Assembled from:
//
//		movq    0(%r14), %r11          // g.stack.lo
//		cmpq    %r11, %rsp
//		jb      foreign
//		cmpq    8(%r14), %rsp          // g.stack.hi
//		jae     foreign
//		movabsq $target, %r11
//		subq    $200, %rsp             // thunkArgArea; call site 16-aligned
//		call    *%r11
//		addq    $200, %rsp
//		jmp     done
//	foreign:
//		movq    -40(%r11), %r13        // previous level top, 0 if none
//		movq    %r13, %r10
//		testq   %r10, %r10
//		jnz     1f
//		leaq    0x2000000(%r11), %r10  // lo + 32 MiB
//	1:	leaq    -0x400000(%r10), %r15  // top for a nested entry
//		cmpq    %r11, %r15
//		ja      2f
//		int3
//	2:	movq    %r15, -40(%r11)
//		movq    %rsp, %r12             // the fiber SP
//		movq    %r10, %rsp
//		pushq   %r13
//		pushq   %r12
//		movabsq $target, %r11
//		subq    $208, %rsp             // thunkArgArea + 8; call site 16-aligned
//		call    *%r11
//		addq    $208, %rsp
//		popq    %r12
//		popq    %r13
//		movq    0(%r14), %r11
//		movq    %r13, -40(%r11)        // release the level
//		movq    %r12, %rsp
//	done:
//		popq    %r15 / %r14 / %r13 / %r12 / %rbp / %rbx
//		ret
var thunkCallTail = []byte{
	0x4d, 0x8b, 0x1e, 0x4c, 0x39, 0xdc, 0x72, 0x23, 0x49, 0x3b, 0x66, 0x08,
	0x73, 0x1d, 0x49, 0xbb, 0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11,
	0x48, 0x81, 0xec, 0xc8, 0x00, 0x00, 0x00, 0x41, 0xff, 0xd3, 0x48, 0x81,
	0xc4, 0xc8, 0x00, 0x00, 0x00, 0xeb, 0x57, 0x4d, 0x8b, 0x6b, 0xd8, 0x4d,
	0x89, 0xea, 0x4d, 0x85, 0xd2, 0x75, 0x07, 0x4d, 0x8d, 0x93, 0x00, 0x00,
	0x00, 0x02, 0x4d, 0x8d, 0xba, 0x00, 0x00, 0xc0, 0xff, 0x4d, 0x39, 0xdf,
	0x77, 0x01, 0xcc, 0x4d, 0x89, 0x7b, 0xd8, 0x49, 0x89, 0xe4, 0x4c, 0x89,
	0xd4, 0x41, 0x55, 0x41, 0x54, 0x49, 0xbb, 0x88, 0x77, 0x66, 0x55, 0x44,
	0x33, 0x22, 0x11, 0x48, 0x81, 0xec, 0xd0, 0x00, 0x00, 0x00, 0x41, 0xff,
	0xd3, 0x48, 0x81, 0xc4, 0xd0, 0x00, 0x00, 0x00, 0x41, 0x5c, 0x41, 0x5d,
	0x4d, 0x8b, 0x1e, 0x4d, 0x89, 0x6b, 0xd8, 0x4c, 0x89, 0xe4, 0x41, 0x5f,
	0x41, 0x5e, 0x41, 0x5d, 0x41, 0x5c, 0x5d, 0x5b, 0xc3,
}

// Offsets of the two imm64 call targets inside thunkCallTail.
const (
	thunkTailTarget1 = 16
	thunkTailTarget2 = 91
)

// guestAltStackSlot is the guard-area slot (lo-40) holding the top of the
// reserved host-call level in use; see thunkCallTail.
const guestAltStackSlot = 40

// mkTrapThunk writes a trampoline that reports name as an unimplemented host
// symbol when it is actually called. namePtr is the address of a NUL-terminated
// C string that must outlive the thunk.
func mkTrapThunk(namePtr uintptr) uintptr {
	b := codeArena(trapThunkLen)
	off := 0
	copy(b[off:], goShimFSPrefix)
	off += len(goShimFSPrefix)
	copy(b[off:], movAXPrefix)
	off += len(movAXPrefix)
	put64(b[off:], namePtr)
	off += 8
	copy(b[off:], movR11Prefix)
	off += len(movR11Prefix)
	put64(b[off:], funcAddr(goTrap))
	off += 8
	emitSubAddImm(b[off:], true, thunkArgArea) // SUBQ $thunkArgArea, SP
	off += 7
	copy(b[off:], []byte{0x41, 0xFF, 0xD3})
	off += 3
	emitSubAddImm(b[off:], false, thunkArgArea) // ADDQ $thunkArgArea, SP
	off += 7
	b[off] = 0xC3
	return uintptr(unsafe.Pointer(&b[0]))
}

// trapNames pins the C strings handed to trap thunks so the garbage collector
// keeps them alive.
var (
	trapNamesMu sync.Mutex
	trapNames   [][]byte
)

// trapThunk returns a stable failing stub for an unimplemented symbol.
func trapThunk(name string) uintptr {
	nb := append([]byte(name), 0)
	trapNamesMu.Lock()
	trapNames = append(trapNames, nb)
	trapNamesMu.Unlock()
	a := mkTrapThunk(uintptr(unsafe.Pointer(&nb[0])))
	trapAddrs[name] = a
	return a
}

// trapAddrs records every trap thunk's arena address by symbol name, for
// diagnostics (attributing a guest jump into the arena to a symbol).
var trapAddrs = map[string]uintptr{}

// DebugTrapMap returns the trap-thunk address ledger (name -> address).
func DebugTrapMap() map[string]uintptr {
	m := make(map[string]uintptr, len(trapAddrs))
	for n, a := range trapAddrs {
		m[n] = a
	}
	return m
}

// ArenaRange returns the first slab's base address, the total bytes written
// across slabs, and the per-slab size, for diagnostics.
func ArenaRange() (base, used, size uintptr) {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	var basePtr uintptr
	total := 0
	if arena == nil {
		return 0, 0, arenaSize
	}
	basePtr = uintptr(unsafe.Pointer(&arena[0]))
	if len(arenas) > 0 {
		basePtr = uintptr(unsafe.Pointer(&arenas[0][0]))
	}
	total = arenaOff
	for _, a := range arenas {
		total += len(a)
	}
	return basePtr, uintptr(total), uintptr(arenaSize)
}

// ArenaDump returns a copy of the written portion of every slab, concatenated
// in address order, for disassembly/debug dumps.
func ArenaDump() []byte {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	out := []byte{}
	for _, a := range arenas { // older slabs first, in address order
		out = append(out, a...)
	}
	if arena != nil {
		out = append(out, arena[:arenaOff]...)
	}
	return out
}
