package hostabi

import (
	"fmt"
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
//	MOVQ FS:-8, R14        ; load the Go g register (ABIInternal)
//	MOVQ DI, AX            ; SysV integer arg 1 -> Go arg 1
//	MOVQ SI, BX            ; 2 -> 2
//	MOVQ CX, DI            ; 4 -> 4  (read before CX is overwritten)
//	MOVQ DX, CX            ; 3 -> 3
//	MOVQ R8, SI            ; 5 -> 5
//	MOVQ R9, R8            ; 6 -> 6
//	MOVQ $target, R11      ; code address of the Go implementation
//	SUBQ $8, SP            ; 16-byte stack alignment for the call
//	CALL R11
//	ADDQ $8, SP
//	RET
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
	thunkSize    = 64
	trapThunkLen = 64
)

var goShimFSPrefix = []byte{0x64, 0x4C, 0x8B, 0x34, 0x25, 0xF8, 0xFF, 0xFF, 0xFF} // MOVQ FS:-8, R14
var movR11Prefix = []byte{0x49, 0xBB}                                             // MOVQ imm64, R11
var movAXPrefix = []byte{0x48, 0xB8}                                              // MOVQ imm64, AX

var (
	arenaMu   sync.Mutex
	arena     []byte
	arenaOff  int
	arenaExec bool
)

const arenaSize = 1 << 20 // 1 MiB of trampolines is far more than the ~600 we need

// codeArena returns a pointer to n writable bytes in the arena, growing the
// arena as needed. The caller writes code, then calls arenaMakeExec.
func codeArena(n int) []byte {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	if arena == nil {
		m, err := syscall.Mmap(-1, 0, arenaSize,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			panic("hostabi: cannot allocate code arena: " + err.Error())
		}
		arena = m
		arenaOff = 0
		arenaExec = false
	}
	if arenaOff+n > len(arena) {
		panic(fmt.Sprintf("hostabi: code arena exhausted (%d bytes)", len(arena)))
	}
	if arenaExec {
		// Re-open the arena for writing; the caller must call arenaMakeExec
		// again before the new thunk can run.
		if err := syscall.Mprotect(arena, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			panic("hostabi: cannot reopen code arena: " + err.Error())
		}
		arenaExec = false
	}
	b := arena[arenaOff : arenaOff+n]
	arenaOff += n
	return b
}

// arenaMakeExec flips the whole arena to read+execute. It is idempotent and
// safe to call while another thunk is being written (the write lock is taken
// again by codeArena).
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
	copy(b[off:], movR11Prefix) // MOVQ target, R11
	off += len(movR11Prefix)
	put64(b[off:], target)
	off += 8
	copy(b[off:], []byte{0x48, 0x83, 0xEC, 0x08}) // SUBQ $8, SP
	off += 4
	copy(b[off:], []byte{0x41, 0xFF, 0xD3}) // CALL R11
	off += 3
	copy(b[off:], []byte{0x48, 0x83, 0xC4, 0x08}) // ADDQ $8, SP
	off += 4
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
	copy(b[off:], []byte{0x48, 0x83, 0xEC, 0x08})
	off += 4
	copy(b[off:], []byte{0x41, 0xFF, 0xD3})
	off += 3
	copy(b[off:], []byte{0x48, 0x83, 0xC4, 0x08})
	off += 4
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
	return mkTrapThunk(uintptr(unsafe.Pointer(&nb[0])))
}
