package hostabi

import (
	"math"
	"runtime"
	"sync"
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

// We never create guest threads, so join/detach cannot succeed; return ESRCH.
//
//go:noinline
func goPthreadJoin(thread, ret uintptr) int { return 3 }
func goPthreadDetach(thread uintptr) int    { return 3 }

// pthread_cond_*: the condition is a flag word in bionic's (44-byte) cond
// storage. cond_wait returns one spurious wakeup after re-acquiring the mutex,
// which POSIX permits, so single-threaded guests never spin forever.
//
//go:noinline
func goCondInit(cond, attr uintptr) int { return 0 }

//go:noinline
func goCondDestroy(cond uintptr) int { return 0 }

//go:noinline
func goCondSignal(cond uintptr) int {
	atomicStoreFlag(cond, 1)
	return 0
}

//go:noinline
func goCondBroadcast(cond uintptr) int {
	atomicStoreFlag(cond, 1)
	return 0
}

//go:noinline
func goCondWait(cond, mutex uintptr) int {
	atomicStoreFlag(cond, 0)
	goPthreadMutexUnlock(mutex)
	goPthreadMutexLock(mutex)
	return 0
}

//go:noinline
func goCondTimedwait(cond, mutex, abstime uintptr) int {
	if abstime != 0 {
		sec := *(*int64)(unsafe.Pointer(abstime))
		nsec := *(*int64)(unsafe.Pointer(abstime + 8))
		now := monotonicNowNanos()
		if sec < now/1e9 || (sec == now/1e9 && nsec <= now%1e9) {
			return 110 // ETIMEDOUT
		}
	}
	return goCondWait(cond, mutex)
}

func atomicStoreFlag(p uintptr, v uint32) {
	*(*uint32)(unsafe.Pointer(p)) = v
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
