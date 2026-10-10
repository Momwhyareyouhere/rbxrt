package hostabi

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// exitNow terminates the process; kept separate so helper files need not import
// os just for this.
func exitNow(code int) { os.Exit(code) }

// callGuest0 and callGuestVoid are implemented in abi_amd64.s.
func callGuest0(fn uintptr) uintptr
func callGuestVoid(fn uintptr)
func callGuestInt3(fn, a, b, c uintptr) int
func callGuestVoid1(fn, a uintptr)
func callHostRaw(fn, a, b, c uintptr) uintptr

// CallGuest0 invokes a guest function with no arguments and returns its result.
func CallGuest0(fn uintptr) uintptr { return callGuest0(fn) }

// CallGuestVoid invokes a guest function with no arguments and no result.
func CallGuestVoid(fn uintptr) { callGuestVoid(fn) }

// CallGuestInt3 invokes a guest function with three integer arguments and
// returns its integer result.
func CallGuestInt3(fn, a, b, c uintptr) int { return callGuestInt3(fn, a, b, c) }

// CallGuestVoid1 invokes a guest function with one integer argument and no
// result. Used for destructor callbacks of the form void (*)(void*).
func CallGuestVoid1(fn, a uintptr) { callGuestVoid1(fn, a) }

// CallHostRaw invokes a host symbol (a thunk address) exactly as guest code
// would, passing up to three integer arguments. It is used for self-tests.
func CallHostRaw(fn, a, b, c uintptr) uintptr { return callHostRaw(fn, a, b, c) }

// cstr reads a NUL-terminated C string at p without creating a Go pointer to
// guest memory.
func cstr(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := int(goStrnlen(p, 1<<20))
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = *(*byte)(unsafe.Pointer(p + uintptr(i)))
	}
	return string(b)
}

// --- fatal exits ---------------------------------------------------------

//go:noinline
func goExit(code uintptr) { os.Exit(int(code)) }

//go:noinline
func goAbort() {
	fmt.Fprintln(os.Stderr, "rbxrt: guest called abort()")
	os.Exit(134)
}

//go:noinline
func goStackChkFail() {
	fmt.Fprintln(os.Stderr, "rbxrt: stack smashing detected (__stack_chk_fail)")
	os.Exit(134)
}

//go:noinline
func goAssert(expr, file uintptr, line uintptr) {
	fmt.Fprintf(os.Stderr, "rbxrt: assertion failed: %s at %s:%d\n", cstr(expr), cstr(file), int(line))
	os.Exit(134)
}

//go:noinline
func goSetAbortMessage(msg uintptr) {
	fmt.Fprintf(os.Stderr, "rbxrt: abort message: %s\n", cstr(msg))
}

// goTrap is the target of trap thunks: it reports the unimplemented symbol and
// aborts. It never returns.
//
//go:noinline
func goTrap(namePtr uintptr) {
	fmt.Fprintf(os.Stderr, "rbxrt: fatal: guest called unimplemented host symbol %q\n", cstr(namePtr))
	os.Exit(70) // EX_SOFTWARE
}

// --- android log ---------------------------------------------------------

//go:noinline
func goAndroidLogWrite(prio, tag, msg uintptr) int {
	writeLog(cstr(tag), cstr(msg))
	return 0
}

//go:noinline
func goAndroidLogPrint(prio, tag, fmtp uintptr, extra uintptr) int {
	// Formatting varargs is not implemented yet; the message is reported
	// verbatim so the failure is visible rather than silent.
	writeLog(cstr(tag), cstr(fmtp))
	return 0
}

//go:noinline
func goAndroidLogBufWrite(bufID, prio, tag, msg uintptr) int {
	writeLog(cstr(tag), cstr(msg))
	return 0
}

func writeLog(tag, msg string) {
	fmt.Fprintf(os.Stderr, "rbxrt: [%s] %s\n", tag, msg)
}

// --- environment ---------------------------------------------------------

//go:noinline
func goGetenv(name uintptr) uintptr { return envGet(cstr(name)) }

//go:noinline
func goSetenv(name, value, overwrite uintptr) int {
	if envSet(cstr(name), cstr(value), overwrite != 0) {
		return 0
	}
	return -1
}

//go:noinline
func goUnsetenv(name uintptr) int {
	envUnset(cstr(name))
	return 0
}

// --- process -------------------------------------------------------------

//go:noinline
func goGetpid() uintptr { return uintptr(os.Getpid()) }

//go:noinline
func goGetppid() uintptr { return uintptr(os.Getppid()) }

//go:noinline
func goGettid() uintptr {
	var tid int
	// SYS_GETTID, no arguments
	r, _, _ := syscall.Syscall(syscall.SYS_GETTID, 0, 0, 0)
	tid = int(r)
	return uintptr(tid)
}

//go:noinline
func goGetuid() uintptr { return uintptr(os.Getuid()) }

//go:noinline
func goGeteuid() uintptr { return uintptr(os.Geteuid()) }

//go:noinline
func goGetpagesize() uintptr { return uintptr(os.Getpagesize()) }

//go:noinline
func goSysconf(name uintptr) int {
	switch int(name) {
	case 30: // _SC_PAGESIZE
		return os.Getpagesize()
	case 39: // _SC_OPEN_MAX
		return 1024
	case 84: // _SC_NPROCESSORS_ONLN
		return runtime.NumCPU()
	default:
		return -1
	}
}

//go:noinline
func goGetauxval(kind uintptr) uintptr { return getauxval(uintptr(kind)) }

// --- time ----------------------------------------------------------------

//go:noinline
func goTime(tloc uintptr) int {
	now := time.Now().Unix()
	if tloc != 0 {
		*(*int64)(unsafe.Pointer(tloc)) = now
	}
	return int(now)
}

//go:noinline
func goGettimeofday(tv uintptr) int {
	now := time.Now()
	sec := now.Unix()
	usec := int64(now.Nanosecond() / 1000)
	if tv != 0 {
		*(*int64)(unsafe.Pointer(tv)) = sec
		*(*int64)(unsafe.Pointer(tv + 8)) = usec
	}
	return 0
}

func isMonotonic(clockid int) bool {
	return clockid == 1 /* CLOCK_MONOTONIC */ || clockid == 4 /* CLOCK_MONOTONIC_RAW */ ||
		clockid == 6 /* CLOCK_MONOTONIC_COARSE */ || clockid == 7 /* CLOCK_BOOTTIME */
}

//go:noinline
func goClockGettime(clockid, tp uintptr) int {
	if tp != 0 {
		var sec, nsec int64
		if isMonotonic(int(clockid)) {
			ns := time.Duration(monotonicNowNanos())
			sec = int64(ns / time.Second)
			nsec = int64(ns % time.Second)
		} else {
			now := time.Now()
			sec = now.Unix()
			nsec = int64(now.Nanosecond())
		}
		*(*int64)(unsafe.Pointer(tp)) = sec
		*(*int64)(unsafe.Pointer(tp + 8)) = nsec
	}
	return 0
}

//go:noinline
func goNanosleep(req, rem uintptr) int {
	if req != 0 {
		sec := *(*int64)(unsafe.Pointer(req))
		nsec := *(*int64)(unsafe.Pointer(req + 8))
		time.Sleep(time.Duration(sec)*time.Second + time.Duration(nsec))
	}
	return 0
}

// --- memory mapping ------------------------------------------------------

//go:noinline
func goMmap(addr, length, prot, flags, fd, off uintptr) uintptr {
	p, err := syscall.Mmap(int(fd), int64(off), int(length), int(prot), int(flags))
	if err != nil {
		return ^uintptr(0) // MAP_FAILED
	}
	return uintptr(unsafe.Pointer(&p[0]))
}

//go:noinline
func goMunmap(addr, length uintptr) int {
	err := syscall.Munmap(unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(length)))
	if err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goMprotect(addr, length, prot uintptr) int {
	if err := syscall.Mprotect(unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(length)), int(prot)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goMadvise(addr, length, advice uintptr) int {
	_ = syscall.Madvise(unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(length)), int(advice))
	return 0
}

// --- file descriptors ----------------------------------------------------

//go:noinline
func goOpen(path, flags, mode uintptr) int {
	fd, err := syscall.Open(cstr(path), int(flags), uint32(mode))
	if err != nil {
		return -1
	}
	return fd
}

//go:noinline
func goClose(fd uintptr) int {
	if err := syscall.Close(int(fd)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goRead(fd, buf, n uintptr) int {
	r, err := syscall.Read(int(fd), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)))
	if err != nil {
		return -1
	}
	return r
}

//go:noinline
func goWrite(fd, buf, n uintptr) int {
	r, err := syscall.Write(int(fd), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)))
	if err != nil {
		return -1
	}
	return r
}

//go:noinline
func goPread(fd, buf, n, off uintptr) int {
	r, err := syscall.Pread(int(fd), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)), int64(off))
	if err != nil {
		return -1
	}
	return r
}

//go:noinline
func goPwrite(fd, buf, n, off uintptr) int {
	r, err := syscall.Pwrite(int(fd), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)), int64(off))
	if err != nil {
		return -1
	}
	return r
}

//go:noinline
func goLseek(fd, off, whence uintptr) int64 {
	r, err := syscall.Seek(int(fd), int64(off), int(whence))
	if err != nil {
		return -1
	}
	return r
}

//go:noinline
func goAccess(path, mode uintptr) int {
	if err := syscall.Access(cstr(path), uint32(mode)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goFcntl(fd, cmd, arg uintptr) int {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, cmd, arg)
	if errno != 0 {
		return -1
	}
	return int(r)
}

//go:noinline
func goGetcwd(buf, size uintptr) uintptr {
	wd, err := os.Getwd()
	if err != nil {
		return 0
	}
	if uintptr(len(wd))+1 > size {
		return 0
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(buf)), len(wd)), wd)
	*(*byte)(unsafe.Pointer(buf + uintptr(len(wd)))) = 0
	return buf
}

//go:noinline
func goMkdir(path, mode uintptr) int {
	if err := syscall.Mkdir(cstr(path), uint32(mode)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goRmdir(path uintptr) int {
	if err := syscall.Rmdir(cstr(path)); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goUnlink(path uintptr) int {
	if err := syscall.Unlink(cstr(path)); err != nil {
		return -1
	}
	return 0
}

// --- pthread (single-threaded semantics) ---------------------------------

// The runtime currently runs guest code on one thread, so mutexes are simple
// atomic spinlocks over the guest's pthread_mutex_t storage. This is correct
// for uncontended use and degrades to yielding under contention. Full thread
// support (pthread_create running guest code) is not implemented.

//go:noinline
func goPthreadMutexInit(m, attr uintptr) int {
	*(*uint32)(unsafe.Pointer(m)) = 0
	return 0
}

//go:noinline
func goPthreadMutexLock(m uintptr) int {
	p := (*uint32)(unsafe.Pointer(m))
	for !atomic.CompareAndSwapUint32(p, 0, 1) {
		runtime.Gosched()
	}
	return 0
}

//go:noinline
func goPthreadMutexTrylock(m uintptr) int {
	if atomic.CompareAndSwapUint32((*uint32)(unsafe.Pointer(m)), 0, 1) {
		return 0
	}
	return 16 // EBUSY
}

//go:noinline
func goPthreadMutexUnlock(m uintptr) int {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(m)), 0)
	return 0
}

//go:noinline
func goPthreadMutexDestroy(m uintptr) int { return 0 }

//go:noinline
func goPthreadMutexattrInit(a uintptr) int { return 0 }

//go:noinline
func goPthreadMutexattrDestroy(a uintptr) int { return 0 }

//go:noinline
func goPthreadMutexattrSettype(a, t uintptr) int { return 0 }

//go:noinline
func goPthreadSelf() uintptr { return uintptr(os.Getpid()) }

//go:noinline
func goPthreadEqual(a, b uintptr) int {
	if a == b {
		return 1
	}
	return 0
}

//go:noinline
func goPthreadOnce(once, fn uintptr) int {
	p := (*uint32)(unsafe.Pointer(once))
	if atomic.CompareAndSwapUint32(p, 0, 1) {
		CallGuestVoid(fn)
	}
	return 0
}

//go:noinline
func goPthreadKeyCreate(key, dtor uintptr) int {
	id := atomic.AddUint32(&tlsKeyNext, 1)
	if id == 0 {
		return 11 // EAGAIN
	}
	*(*uint32)(unsafe.Pointer(key)) = id
	return 0
}

//go:noinline
func goPthreadKeyDelete(key uintptr) int { return 0 }

//go:noinline
func goPthreadGetspecific(key uintptr) uintptr { return 0 }

//go:noinline
func goPthreadSetspecific(key, val uintptr) int { return 0 }

var tlsKeyNext uint32

//go:noinline
func goPthreadSetnameNp(thread, name uintptr) int { return 0 }

//go:noinline
func goPthreadSigmask(how, set, old uintptr) int { return 0 }

//go:noinline
func goPthreadAttrInit(a uintptr) int {
	// pthread_attr_t is 56 bytes on x86-64 bionic; zero it.
	for i := uintptr(0); i < 56; i += 8 {
		*(*uint64)(unsafe.Pointer(a + i)) = 0
	}
	return 0
}

//go:noinline
func goPthreadAttrDestroy(a uintptr) int { return 0 }

//go:noinline
func goPthreadAttrSetdetachstate(a, s uintptr) int { return 0 }

//go:noinline
func goPthreadAttrSetstacksize(a, s uintptr) int { return 0 }

//go:noinline
func goZeroRet0() int { return 0 }

//go:noinline
func goZeroRet0Ptr() uintptr { return 0 }

//go:noinline
func goPthreadCreate(thread, attr, start, arg uintptr) int {
	// Running guest code on a second thread is not implemented. Fail loudly
	// rather than pretending the thread was created.
	fmt.Fprintln(os.Stderr, "rbxrt: fatal: guest called pthread_create (thread support is not implemented)")
	os.Exit(70)
	return -1
}

//go:noinline
func goSemInit(sem, pshared, value uintptr) int {
	*(*uint32)(unsafe.Pointer(sem)) = uint32(value)
	return 0
}

//go:noinline
func goSemWait(sem uintptr) int {
	p := (*uint32)(unsafe.Pointer(sem))
	for {
		v := atomic.LoadUint32(p)
		if v > 0 && atomic.CompareAndSwapUint32(p, v, v-1) {
			return 0
		}
		runtime.Gosched()
	}
}

//go:noinline
func goSemPost(sem uintptr) int {
	atomic.AddUint32((*uint32)(unsafe.Pointer(sem)), 1)
	return 0
}

//go:noinline
func goSemDestroy(sem uintptr) int { return 0 }
