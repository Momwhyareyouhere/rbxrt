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

	// A host thunk is ~100 bytes of code plus the argument area it reserves;
	// 128 keeps them naturally spaced in the arena and leaves room for growth.
	thunkSize    = 128
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
	copy(b[off:], movR11Prefix) // MOVQ target, R11
	off += len(movR11Prefix)
	put64(b[off:], target)
	off += 8
	emitSubAddImm(b[off:], true, thunkArgArea) // SUBQ $thunkArgArea, SP
	off += 7
	copy(b[off:], []byte{0x41, 0xFF, 0xD3}) // CALL R11
	off += 3
	emitSubAddImm(b[off:], false, thunkArgArea) // ADDQ $thunkArgArea, SP
	off += 7
	// Restore the guest's callee-saved registers, in reverse order.
	copy(b[off:], []byte{0x41, 0x5F}) // POPQ R15
	off += 2
	copy(b[off:], []byte{0x41, 0x5E}) // POPQ R14
	off += 2
	copy(b[off:], []byte{0x41, 0x5D}) // POPQ R13
	off += 2
	copy(b[off:], []byte{0x41, 0x5C}) // POPQ R12
	off += 2
	copy(b[off:], []byte{0x5D}) // POPQ RBP
	off++
	copy(b[off:], []byte{0x5B}) // POPQ RBX
	off++
	if off >= thunkSize {
		panic("hostabi: host thunk overflowed its code buffer")
	}
	b[off] = 0xC3 // RET
	return uintptr(unsafe.Pointer(&b[0]))
}

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
