package hostabi

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// This file implements the second tier of host functions: stat family, the
// atexit registry, errno access, conditions, and math decomposers. Each is a
// normal Go function reached through a thunk.

// --- stat family ---------------------------------------------------------

// goCopyInto copies src's memory image into guest memory. The layout of Go's
// syscall.Stat_t matches the x86-64 bionic struct stat for the fields bionic
// uses, so a straight byte copy is correct. The source is a pointer because an
// interface value is a (type, data) pair: copying through &src would copy the
// pair's header rather than the struct it holds, which silently filled the
// guest's stat buffer with nonsense - the mode field then read as "unknown
// file type", which is how this was found.
func goCopyInto(dst uintptr, src unsafe.Pointer, size uintptr) {
	copy(unsafe.Slice((*byte)(unsafe.Pointer(dst)), size), unsafe.Slice((*byte)(src), size))
}

//go:noinline
//go:nosplit
func goStat(path, buf uintptr) int {
	r := sysRet(rawSysNoEnter(4, path, buf, 0))
	if debugFS {
		traceFS("stat", path, r)
	}
	return r
}

//go:noinline
//go:nosplit
func goLstat(path, buf uintptr) int {
	r := sysRet(rawSysNoEnter(6, path, buf, 0))
	if debugFS {
		traceFS("lstat", path, r)
	}
	return r
}

//go:noinline
//go:nosplit
func goFstat(fd, buf uintptr) int {
	if r := rawSysNoEnter(5, fd, buf, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goReadlink(path, buf, size uintptr) uintptr {
	n := rawSysNoEnter(89, path, buf, size)
	if int64(n) < 0 {
		setErrno(int32(-int64(n)))
		return ^uintptr(0)
	}
	return n
}

//go:noinline
func goFtruncate(fd, length uintptr) int {
	if r := rawSysNoEnter(77, fd, length, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goFchmod(fd, mode uintptr) int {
	if r := rawSysNoEnter(91, fd, mode, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goFchown(fd, uid, gid uintptr) int {
	if r := rawSysNoEnter6(93, fd, uid, gid, 0, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goRename(oldp, newp uintptr) int {
	if r := rawSysNoEnter(82, oldp, newp, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goRemove(path uintptr) int {
	if r := rawSysNoEnter(87, path, 0, 0); int64(r) >= 0 || int64(r) == -int64(syscall.ENOENT) {
		return 0
	}
	if r := rawSysNoEnter(84, path, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

// >4-argument syscalls (mremap) go through a raw call with the sixth register.
//
//go:noinline
func goMremap(old, oldSize, newSize, flags uintptr) uintptr {
	r := rawSysNoEnter6(25, old, oldSize, newSize, flags, 0, 0)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return ^uintptr(0)
	}
	return r
}

// --- errno ---------------------------------------------------------------

// debugFS traces filesystem lookups by path (RBXRT_DEBUG_FS): open, fopen,
// stat, lstat and access, with the result. It is how an rbxasset:// path that
// resolves to the wrong place on the host shows up.
var debugFS = os.Getenv("RBXRT_DEBUG_FS") != ""

// traceFread logs one fread under RBXRT_DEBUG_FS: the descriptor (matching
// the fd an earlier fopen trace returned), bytes asked for and bytes read.
func traceFread(fd, want, got int) {
	writeStderrRaw(fmt.Sprintf("rbxrt: fs: fread(fd=%d, %d) -> %d\n", fd, want, got))
}

func traceFS(op string, path uintptr, r int) {
	if !debugFS || path == 0 {
		return
	}
	if r < 0 {
		writeStderrRaw(fmt.Sprintf("rbxrt: fs: %s(%q) -> -1 errno=%d\n", op, cstr(path), *errnoPtr()))
		return
	}
	writeStderrRaw(fmt.Sprintf("rbxrt: fs: %s(%q) -> %d\n", op, cstr(path), r))
}

// bionic's __errno() returns the address of the *calling thread's* errno. The
// engine is heavily multithreaded (curl, for one, reads errno right after a
// non-blocking connect), so a process-wide variable lets one thread's failure
// clobber another's. Each guest thread keeps its errno in its guest stack's
// guard area, beside the call-site capture slots (see guestCaptureRetPC):
// that memory is mapped, private to the thread and stable for its lifetime.
// Code running before any guest stack exists falls back to errnoFallback.
const guestErrnoSlot = 32 // int32 at base - 32

var errnoFallback int32

func errnoPtr() *int32 {
	g := getg()
	gsMu.Lock()
	gs := gsMap[g]
	gsMu.Unlock()
	if gs == nil || gs.base == 0 {
		return &errnoFallback
	}
	return (*int32)(unsafe.Pointer(gs.base - guestErrnoSlot))
}

// setErrno stores e in the calling guest thread's errno.
func setErrno(e int32) { *errnoPtr() = e }

// sysRet converts a raw Linux syscall result into the libc convention the
// engine expects: -1 with errno set on failure (the kernel returns -errno in
// the range [-4095, -1]), the result unchanged otherwise.
func sysRet(r uintptr) int {
	if v := int64(r); v < 0 && v > -4096 {
		setErrno(int32(-v))
		return -1
	}
	return int(r)
}

//go:noinline
func goErrnoAddr() uintptr { return uintptr(unsafe.Pointer(errnoPtr())) }

//go:noinline
func goStrerror(err uintptr) uintptr {
	msg := syscall.Errno(err).Error()
	if msg == "" {
		msg = "Unknown error"
	}
	return cstringConst(msg)
}

// --- atexit --------------------------------------------------------------

type atexitEntry struct{ fn, arg, dso uintptr }

var (
	atexitMu   sync.Mutex
	atexitRegs []atexitEntry
)

//go:noinline
func goCxaAtexit(fn, arg, dso uintptr) int {
	atexitMu.Lock()
	atexitRegs = append(atexitRegs, atexitEntry{fn, arg, dso})
	atexitMu.Unlock()
	return 0
}

// goCxaFinalize runs registered destructors with a matching DSO handle in
// reverse order, then removes them, as bionic does.
//
//go:noinline
func goCxaFinalize(dso uintptr) int {
	atexitMu.Lock()
	run := make([]atexitEntry, 0, len(atexitRegs))
	kept := atexitRegs[:0]
	for _, e := range atexitRegs {
		if dso == 0 || e.dso == dso || e.dso == 0 {
			run = append(run, e)
		} else {
			kept = append(kept, e)
		}
	}
	atexitRegs = kept
	atexitMu.Unlock()
	for i := len(run) - 1; i >= 0; i-- {
		CallGuestVoid1(run[i].fn, run[i].arg)
	}
	return 0
}

//go:noinline
func goRegisterAtfork(pre, parent, child, dso uintptr) int { return 0 }

//go:noinline
func goPthreadAtfork(pre, parent, child uintptr) int { return 0 }

// --- sched / threads -----------------------------------------------------

//go:noinline
func goSchedYield() int {
	runtime.Gosched()
	return 0
}

//go:noinline
func goPthreadJoin(threadp, retp uintptr) int {
	threadMu.Lock()
	t := threads[threadp]
	threadMu.Unlock()
	if t == nil {
		return 3 // ESRCH
	}
	<-t.done
	if retp != 0 {
		*(*uintptr)(unsafe.Pointer(retp)) = t.ret
	}
	threadMu.Lock()
	delete(threads, threadp)
	threadMu.Unlock()
	return 0
}

//go:noinline
func goPthreadDetach(threadp uintptr) int {
	threadMu.Lock()
	t := threads[threadp]
	if t != nil {
		t.detached = true
	}
	threadMu.Unlock()
	if t == nil {
		return 3 // ESRCH
	}
	return 0
}

// pthread_cond_*: bionic's condvar is storage the guest only touches through
// these entry points (statically initialised, or via cond_init), so its first
// 4 bytes double as a generation counter used as a futex word. Signal/broadcast
// bump the counter and wake waiters; wait blocks in the kernel on the counter
// value read before releasing the mutex, so a signal either changes the value
// (making the wait return immediately) or wakes the blocked waiter - never
// lost. timedwait uses FUTEX_WAIT_BITSET with FUTEX_CLOCK_REALTIME, because
// pthread_cond_timedwait's abstime is an absolute CLOCK_REALTIME deadline.
//
// The counter scheme requires the word to be 4-byte aligned (pthread_cond_t
// is) and not otherwise interpreted by the guest (it is not).

//go:noinline
func goCondInit(cond, attr uintptr) int {
	*futexWord(cond) = 0
	return 0
}

//go:noinline
func goCondDestroy(cond uintptr) int { return 0 }

//go:noinline
func goCondSignal(cond uintptr) int {
	atomic.AddUint32(futexWord(cond), 1)
	futexDo(cond, futexOpWake, 1, 0)
	return 0
}

//go:noinline
func goCondBroadcast(cond uintptr) int {
	atomic.AddUint32(futexWord(cond), 1)
	futexDo(cond, futexOpWake, 1<<31-1, 0)
	return 0
}

//go:noinline
func goCondWait(cond, mutex uintptr) int {
	v := atomic.LoadUint32(futexWord(cond))
	depth := mutexUnlockForWait(mutex)
	futexDo(cond, futexOpWait, uintptr(v), 0)
	mutexRelockAfterWait(mutex, depth)
	return 0
}

//go:noinline
func goCondTimedwait(cond, mutex, abstime uintptr) int {
	v := atomic.LoadUint32(futexWord(cond))
	depth := mutexUnlockForWait(mutex)
	var e syscall.Errno
	if abstime == 0 {
		e = futexDo(cond, futexOpWait, uintptr(v), 0)
	} else {
		e = futexDo(cond, futexOpWaitBitset|futexClockRealtime, uintptr(v), abstime)
	}
	mutexRelockAfterWait(mutex, depth)
	if e == syscall.ETIMEDOUT {
		return 110
	}
	return 0
}

// Futex operations and the shared generation word.
const (
	futexOpWait         = 0          // FUTEX_WAIT
	futexOpWake         = 1          // FUTEX_WAKE
	futexOpWaitBitset   = 9          // FUTEX_WAIT_BITSET
	futexClockRealtime  = 256        // FUTEX_CLOCK_REALTIME
	futexBitsetMatchAny = 0xffffffff // FUTEX_BITSET_MATCH_ANY
	futexPrivate        = 128        // FUTEX_PRIVATE_FLAG
)

func futexWord(cond uintptr) *uint32 { return (*uint32)(unsafe.Pointer(cond)) }

// futexDo performs a futex syscall. val3 (the bitset) is only meaningful for
// FUTEX_WAIT_BITSET and is ignored by the other operations.
func futexDo(uaddr, op, val, timeout uintptr) syscall.Errno {
	r := rawSysNoEnter6(syscall.SYS_FUTEX, uaddr, op, val, timeout, 0, futexBitsetMatchAny)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return syscall.Errno(-int64(r))
	}
	return 0
}

//go:noinline
func goCondattrInit(cond, attr uintptr) int { return 0 }
func goCondattrDestroy(attr uintptr) int    { return 0 }

// --- math decomposers ----------------------------------------------------

//go:noinline
func goLdexp(x float64, e int) float64 { return math.Ldexp(x, e) }

// modf(x, *iptr): returns fractional part, stores integral part at iptr.
//
//go:noinline
func goModf(x float64, iptr uintptr) float64 {
	ip := math.Trunc(x)
	*(*float64)(unsafe.Pointer(iptr)) = ip
	return x - ip
}

// frexp(x, *exp): returns fraction, stores exponent at exp.
//
//go:noinline
func goFrexp(x float64, exp uintptr) float64 {
	f, e := math.Frexp(x)
	*(*int)(unsafe.Pointer(exp)) = e
	return f
}

//go:noinline
func goFabsX(x float64) float64 { return math.Abs(x) }

// --- assert / log --------------------------------------------------------

// __android_log_assert(cond, tag, fmt, ...) aborts when cond is false. We
// cannot format the varargs, so the message is reported verbatim.
//
//go:noinline
func goAndroidLogAssert(cond, tag, fmtp, extra uintptr) int {
	writeLog(cstr(tag), cstr(fmtp))
	exitNow(134)
	return 0
}
