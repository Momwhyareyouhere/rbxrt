package hostabi

import (
	"math"
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
// uses, so a straight byte copy is correct.
func goCopyInto(dst uintptr, src any) {
	sz := unsafe.Sizeof(src)
	sp := uintptr(unsafe.Pointer(&src))
	for i := uintptr(0); i < sz; i++ {
		*(*byte)(unsafe.Pointer(dst + i)) = *(*byte)(unsafe.Pointer(sp + i))
	}
}

//go:noinline
func goStat(path, buf uintptr) int {
	var st syscall.Stat_t
	if err := syscall.Stat(cstr(path), &st); err != nil {
		return -1
	}
	goCopyInto(buf, st)
	return 0
}

//go:noinline
func goLstat(path, buf uintptr) int {
	var st syscall.Stat_t
	if err := syscall.Lstat(cstr(path), &st); err != nil {
		return -1
	}
	goCopyInto(buf, st)
	return 0
}

//go:noinline
func goFstat(fd, buf uintptr) int {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return -1
	}
	goCopyInto(buf, st)
	return 0
}

//go:noinline
func goReadlink(path, buf, size uintptr) uintptr {
	n, err := syscall.Readlink(cstr(path), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(size)))
	if err != nil {
		return ^uintptr(0)
	}
	return uintptr(n)
}

//go:noinline
func goFtruncate(fd, length uintptr) int {
	if err := syscall.Ftruncate(int(fd), int64(length)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goFchmod(fd, mode uintptr) int {
	if err := syscall.Fchmod(int(fd), uint32(mode)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goFchown(fd, uid, gid uintptr) int {
	if err := syscall.Fchown(int(fd), int(uid), int(gid)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goRename(oldp, newp uintptr) int {
	if err := syscall.Rename(cstr(oldp), cstr(newp)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goRemove(path uintptr) int {
	p := cstr(path)
	if err := syscall.Unlink(p); err == nil || err == syscall.ENOENT {
		return 0
	}
	if err := syscall.Rmdir(p); err != nil {
		return -1
	}
	return 0
}

// >4-argument syscalls (mremap) go through a raw call with the sixth register.
//
//go:noinline
func goMremap(old, oldSize, newSize, flags uintptr) uintptr {
	r, _, errno := syscall.Syscall6(syscall.SYS_MREMAP, old, oldSize, newSize, flags, 0, 0)
	if errno != 0 {
		return ^uintptr(0)
	}
	return r
}

// --- errno ---------------------------------------------------------------

// errnoVar backs bionic's __errno(): a function returning the address of the
// calling thread's errno. With a single thread a plain variable suffices.
var errnoVar int32

//go:noinline
func goErrnoAddr() uintptr { return uintptr(unsafe.Pointer(&errnoVar)) }

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
	goPthreadMutexUnlock(mutex)
	futexDo(cond, futexOpWait, uintptr(v), 0)
	goPthreadMutexLock(mutex)
	return 0
}

//go:noinline
func goCondTimedwait(cond, mutex, abstime uintptr) int {
	v := atomic.LoadUint32(futexWord(cond))
	goPthreadMutexUnlock(mutex)
	var e syscall.Errno
	if abstime == 0 {
		e = futexDo(cond, futexOpWait, uintptr(v), 0)
	} else {
		e = futexDo(cond, futexOpWaitBitset|futexClockRealtime, uintptr(v), abstime)
	}
	goPthreadMutexLock(mutex)
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
)

func futexWord(cond uintptr) *uint32 { return (*uint32)(unsafe.Pointer(cond)) }

// futexDo performs a futex syscall. val3 (the bitset) is only meaningful for
// FUTEX_WAIT_BITSET and is ignored by the other operations.
func futexDo(uaddr, op, val, timeout uintptr) syscall.Errno {
	_, _, e := syscall.Syscall6(syscall.SYS_FUTEX, uaddr, op, val, timeout, 0, futexBitsetMatchAny)
	return e
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
