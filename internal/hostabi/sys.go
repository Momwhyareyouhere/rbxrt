package hostabi

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// exitNow terminates the process; kept separate so helper files need not import
// os just for this.
func exitNow(code int) { os.Exit(code) }

// guestBias is the load bias of the primary guest image, used only to make
// guest addresses readable in diagnostics (see guestAddr). It does not affect
// behaviour.
var guestBias uintptr

// SetGuestBias records the primary guest image's load bias for diagnostics.
// The loader calls this before any guest code runs.
func SetGuestBias(b uintptr) { guestBias = b }

// PatchGuest overwrites bytes in the guest image. It exists only for
// instrumentation experiments (the text segment is mapped read-only); it
// temporarily makes the containing pages writable and restores RX afterwards.
func PatchGuest(addr uintptr, b []byte) error {
	const pageSize = 0x1000
	start := addr &^ uintptr(pageSize-1)
	end := (addr + uintptr(len(b)) + pageSize - 1) &^ uintptr(pageSize-1)
	region := unsafe.Slice((*byte)(unsafe.Pointer(start)), int(end-start))
	if err := syscall.Mprotect(region, syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC); err != nil {
		return err
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(addr)), len(b)), b)
	return syscall.Mprotect(region, syscall.PROT_READ|syscall.PROT_EXEC)
}

// guestAddr renders an address as "+0x<off>" relative to the guest image when
// it lies inside it, and as a raw address otherwise.
func guestAddr(a uintptr) string {
	if guestBias != 0 && a >= guestBias {
		return fmt.Sprintf("+0x%x", a-guestBias)
	}
	return fmt.Sprintf("%#x", a)
}

// callGuest0 and callGuestVoid are implemented in abi_amd64.s.
func callGuest0(fn uintptr) uintptr
func callGuestVoid(fn uintptr)
func callGuestInt3(fn, a, b, c uintptr) int
func callGuestVoid1(fn, a uintptr)
func callHostRaw(fn, a, b, c uintptr) uintptr

// readFS28 is implemented in abi_amd64.s and used by tests that check TLS stability.
func readFS28() uint64

// guestReturnAddrFromSp was replaced by the per-goroutine capture recorded by
// every host thunk. See guestCapture (stack.go), thunk.go and goStackChkFail.

// readFSbase is implemented in abi_amd64.s and returns the current value of the
// FS base register.
func readFSbase() uintptr

// writeFSbase is implemented in abi_amd64.s and sets the FS base register to addr.
func writeFSbase(addr uintptr)

// clobberCalleeSaved and thunkPreserveProbe are implemented in abi_amd64.s and
// back TestThunkPreservesCalleeSaved, which checks that host thunks restore the
// guest's SysV callee-saved registers across a call.
func clobberCalleeSaved()
func thunkPreserveProbe(thunk uintptr) uintptr

// CallGuest0 invokes a guest function with no arguments and returns its result.
func CallGuest0(fn uintptr) uintptr {
	return invokeGuest(fn, 0, 0, 0)
}

// CallGuestVoid invokes a guest function with no arguments and no result.
func CallGuestVoid(fn uintptr) {
	invokeGuest(fn, 0, 0, 0)
}

// CallGuestInt3 invokes a guest function with three integer arguments and
// returns its integer result.
func CallGuestInt3(fn, a, b, c uintptr) int {
	return int(invokeGuest(fn, a, b, c))
}

// CallGuestInt8 invokes a guest function with eight integer arguments and
// returns its integer result. It exists to drive JNI native methods that take
// six Java parameters: env, thiz, then a..f, with g and h passed on the stack.
func CallGuestInt8(fn, a, b, c, d, e, f, g, h uintptr) int {
	return int(invokeGuest8(fn, a, b, c, d, e, f, g, h))
}

// CallGuestVoid1 invokes a guest function with one integer argument and no
// result. Used for destructor callbacks of the form void (*)(void*).
func CallGuestVoid1(fn, a uintptr) {
	invokeGuest(fn, a, 0, 0)
}

// CallHostRaw invokes a host symbol (a thunk address) exactly as guest code
// would, passing up to three integer arguments. It is used for self-tests.
//
// It runs under a guest-stack context because every thunk records its caller's
// return address just below g.stack.lo (see mkThunk); from Go code that would
// be below the Go stack. See withGuestStackContext.
func CallHostRaw(fn, a, b, c uintptr) uintptr {
	return withGuestStackContext(func() uintptr { return callHostRaw(fn, a, b, c) })
}

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
func goExit(code uintptr) {
	retPC, _ := guestCapture()
	fmt.Fprintf(os.Stderr, "rbxrt: guest called exit(%d) at guest pc %#x (image+%#x)\n",
		int(code), retPC, retPC-guestBias)
	if debugWalk {
		walkGuestFrames(48)
	}
	if debugMem {
		walkGuestFrames(12)
		dumpRawGuestStack(currentSP(), 0x1000, 60)
	}
	os.Exit(int(code))
}

//go:noinline
func goAbort() {
	retPC, _ := guestCapture()
	fmt.Fprintf(os.Stderr, "rbxrt: guest called abort() at guest pc %#x\n", retPC)
	os.Exit(134)
}

//go:noinline
func goStackChkFail() {
	retPC, _ := guestCapture()
	fmt.Fprintf(os.Stderr, "rbxrt: stack smashing detected (__stack_chk_fail) at guest pc %#x\n", retPC)
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
func goAndroidLogPrint(prio, tag, fmtp, a1, a2, a3 uintptr) int {
	writeLog(cstr(tag), formatLogV(cstr(fmtp), [3]uintptr{a1, a2, a3}))
	return 0
}

// maxLogString bounds a %s conversion; a log line is diagnostic, not data.
const maxLogString = 4096

// formatLogV renders the printf-style message of __android_log_print. The
// engine passes its varargs in rcx/r8/r9 and then on the guest stack, as the
// SysV ABI says; the thunk records the guest SP at entry, so stack varargs are
// reachable at sp+8, sp+16, and so on. Floating-point varargs arrive in xmm
// registers, which the call thunk does not capture, so those are rendered as a
// visible placeholder instead of a silently wrong number.
func formatLogV(format string, reg [3]uintptr) string {
	var b strings.Builder
	stack := guestCapturedRSP()
	next := 0
	// arg returns the next integer/pointer vararg: register arguments first,
	// then the guest stack.
	arg := func() uintptr {
		i := next
		next++
		if i < len(reg) {
			return reg[i]
		}
		if stack == 0 {
			return 0
		}
		return *(*uintptr)(unsafe.Pointer(stack + 8 + uintptr(i-len(reg))*8))
	}
	for i := 0; i < len(format); {
		if format[i] != '%' {
			b.WriteByte(format[i])
			i++
			continue
		}
		specStart := i
		j := i + 1
		if j < len(format) && format[j] == '%' {
			b.WriteByte('%')
			i = j + 1
			continue
		}
		fs := j
		for j < len(format) && strings.IndexByte("-+ #0", format[j]) >= 0 {
			j++
		}
		flags := format[fs:j]
		width := -1
		if j < len(format) && format[j] == '*' {
			width = int(int32(uint32(arg())))
			j++
		} else {
			ws := j
			for j < len(format) && format[j] >= '0' && format[j] <= '9' {
				j++
			}
			if j > ws {
				if n, err := strconv.Atoi(format[ws:j]); err == nil {
					width = n
				}
			}
		}
		prec := -1
		if j < len(format) && format[j] == '.' {
			j++
			prec = 0
			if j < len(format) && format[j] == '*' {
				prec = int(int32(uint32(arg())))
				j++
			} else {
				ps := j
				for j < len(format) && format[j] >= '0' && format[j] <= '9' {
					j++
				}
				if j > ps {
					if n, err := strconv.Atoi(format[ps:j]); err == nil {
						prec = n
					}
				}
			}
		}
		ls := j
		for j < len(format) && strings.IndexByte("hlLjztq", format[j]) >= 0 {
			j++
		}
		length := format[ls:j]
		if j >= len(format) {
			b.WriteString(format[specStart:]) // truncated conversion
			break
		}
		conv := format[j]
		i = j + 1
		wide := strings.ContainsAny(length, "lLzjtq")
		switch conv {
		case 'd', 'i':
			v := arg()
			text := strconv.FormatInt(int64(int32(uint32(v))), 10)
			if wide {
				text = strconv.FormatInt(int64(v), 10)
			}
			b.WriteString(padNumber(text, width, prec, flags))
		case 'u', 'o', 'x', 'X':
			v := arg()
			base := 10
			switch conv {
			case 'o':
				base = 8
			case 'x', 'X':
				base = 16
			}
			if !wide {
				v = uintptr(uint32(v))
			}
			text := strconv.FormatUint(uint64(v), base)
			if conv == 'X' {
				text = strings.ToUpper(text)
			}
			if prec >= 0 && len(text) < prec {
				text = strings.Repeat("0", prec-len(text)) + text
			}
			b.WriteString(padNumber(text, width, -1, flags))
		case 'c':
			b.WriteByte(byte(arg()))
		case 's':
			s := cstrN(arg(), maxLogString)
			if prec >= 0 && len(s) > prec {
				s = s[:prec]
			}
			b.WriteString(padString(s, width, flags))
		case 'p':
			b.WriteString("0x" + strconv.FormatUint(uint64(arg()), 16))
		case 'f', 'F', 'e', 'E', 'g', 'G', 'a', 'A':
			// xmm varargs are not captured by the call thunk.
			b.WriteString("<float?>")
		default:
			b.WriteString(format[specStart : j+1]) // reproduce unknown specs
		}
	}
	return b.String()
}

// padNumber applies the '-'/width/'0' flags to an already-formatted number.
func padNumber(text string, width, prec int, flags string) string {
	if prec >= 0 && len(text) < prec && !strings.Contains(flags, "-") {
		text = strings.Repeat("0", prec-len(text)) + text
	}
	if width <= len(text) {
		return text
	}
	pad := width - len(text)
	if strings.Contains(flags, "-") {
		return text + strings.Repeat(" ", pad)
	}
	if strings.Contains(flags, "0") && prec < 0 {
		sign := ""
		if len(text) > 0 && (text[0] == '-' || text[0] == '+') {
			sign, text = text[:1], text[1:]
		}
		return sign + strings.Repeat("0", pad) + text
	}
	return strings.Repeat(" ", pad) + text
}

// padString applies the '-'/width flags to a string conversion.
func padString(text string, width int, flags string) string {
	if width <= len(text) {
		return text
	}
	pad := strings.Repeat(" ", width-len(text))
	if strings.Contains(flags, "-") {
		return text + pad
	}
	return pad + text
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

// goSysconf implements Bionic's sysconf(3). The numeric _SC_* values are
// Bionic's, not glibc's (see bionic libc/include/bits/sysconf.h): for example
// _SC_PAGESIZE is 0x27 and _SC_NPROCESSORS_CONF is 0x60. Returning -1 for a
// constant the guest treats as a bound (the CPU count) would turn a simple loop
// into an effectively infinite one.
//
//go:noinline
func goSysconf(name uintptr) int {
	switch int(name) {
	case 0x06: // _SC_CLK_TCK
		return 100
	case 0x0b: // _SC_OPEN_MAX
		var rl syscall.Rlimit
		if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil {
			return int(rl.Cur)
		}
		return 1024
	case 0x1b: // _SC_STREAM_MAX
		return 16
	case 0x25: // _SC_ATEXIT_MAX
		return 4096
	case 0x26: // _SC_IOV_MAX
		return 1024
	case 0x27: // _SC_PAGESIZE
		return os.Getpagesize()
	case 0x28: // _SC_PAGE_SIZE
		return os.Getpagesize()
	case 0x3c, 0x3d: // _SC_MEMLOCK, _SC_MEMLOCK_RANGE
		return 1
	case 0x4a: // _SC_THREAD_DESTRUCTOR_ITERATIONS
		return 4
	case 0x4b: // _SC_THREAD_KEYS_MAX
		return 128
	case 0x4c: // _SC_THREAD_STACK_MIN
		return 16384
	case 0x4f: // _SC_THREADS
		return 1
	case 0x60: // _SC_NPROCESSORS_CONF
		return runtime.NumCPU()
	case 0x61: // _SC_NPROCESSORS_ONLN
		return runtime.NumCPU()
	case 0x62: // _SC_PHYS_PAGES
		return physPages(false)
	case 0x63: // _SC_AVPHYS_PAGES
		return physPages(true)
	case 0x64: // _SC_MONOTONIC_CLOCK
		return 1
	default:
		// Most remaining queries are POSIX option checks: a positive value
		// means supported, -1 means not.
		return -1
	}
}

// physPages returns the number of physical (or available) pages, as
// _SC_PHYS_PAGES / _SC_AVPHYS_PAGES require.
func physPages(avail bool) int {
	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err != nil {
		return 0
	}
	unit := uint64(si.Unit)
	if unit == 0 {
		unit = 1
	}
	n := si.Totalram
	if avail {
		n = si.Freeram
	}
	return int(n * unit / uint64(os.Getpagesize()))
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
	if debugClock {
		clockGetCount++
		if clockGetCount == 1000000 || clockGetCount == 3000000 {
			rp, rbp := guestCapture()
			fmt.Fprintf(os.Stderr, "rbxrt: clock_gettime caller=%#x (+%#x) n=%d rbp=%#x\n", rp, rp-guestBias, clockGetCount, rbp)
			walkGuestFrames(40)
		}
	}
	if tp != 0 {
		var sec, nsec int64
		if isMonotonic(int(clockid)) {
			ns := time.Duration(hostMonotonicNowNanos(int(clockid)))
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
		tracePipeIO("read", fd, buf, n, -1)
		return -1
	}
	tracePipeIO("read", fd, buf, n, r)
	return r
}

//go:noinline
func goWrite(fd, buf, n uintptr) int {
	r, err := syscall.Write(int(fd), unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)))
	if err != nil {
		tracePipeIO("write", fd, buf, n, -1)
		return -1
	}
	tracePipeIO("write", fd, buf, n, r)
	return r
}

// debugPipe traces short reads and writes (RBXRT_DEBUG_PIPE). The engine drives
// its lifecycle through a byte-at-a-time command pipe, so the sequence of bytes
// and the guest call sites that produce them is the clearest view of how far a
// launch got and who is still waiting.
var debugPipe = os.Getenv("RBXRT_DEBUG_PIPE") != ""

func tracePipeIO(op string, fd, buf, n uintptr, r int) {
	if !debugPipe || n == 0 || n > 64 || (fd <= 2 && op == "write") {
		return
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = *(*byte)(unsafe.Pointer(buf + uintptr(i)))
	}
	retPC, _ := guestCapture()
	fmt.Fprintf(os.Stderr, "rbxrt: pipe %s fd=%d n=%d [% x] by image+%#x -> %d\n",
		op, fd, n, b, GuestImageOffset(retPC), r)
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
func goPthreadSelf() uintptr {
	if mainThreadOverride != 0 {
		return mainThreadOverride
	}
	return getg()
}

// For forcing thread identity from outside the package.
var mainThreadOverride uintptr

// SetMainThreadID forces pthread_self() to return id for all threads.
func SetMainThreadID(id uintptr) {
	// use atomic if available via runtime; simple store is fine for now
	// best effort
	mainThreadOverride = id
}

// GetG returns the current g pointer.
func GetG() uintptr { return getg() }


//go:noinline
func goPthreadEqual(a, b uintptr) int {
	if a == b {
		return 1
	}
	return 0
}

// goPthreadOnce implements pthread_once with the usual three states: 0 not
// started, 1 running, 2 done. A bare CAS(0,1) around the initialiser (as an
// earlier version did) returns success to a second caller while the first is
// still inside fn, so that caller can observe a half-constructed object.
//
//go:noinline
func goPthreadOnce(once, fn uintptr) int {
	p := (*uint32)(unsafe.Pointer(once))
	for {
		switch atomic.LoadUint32(p) {
		case 2:
			return 0
		case 0:
			if atomic.CompareAndSwapUint32(p, 0, 1) {
				CallGuestVoid(fn)
				atomic.StoreUint32(p, 2)
				return 0
			}
		default: // 1: another thread is running fn
			runtime.Gosched()
		}
	}
}

//go:noinline
func goPthreadKeyCreate(key, dtor uintptr) int {
	id := atomic.AddUint32(&tlsKeyNext, 1)
	if id == 0 {
		return 11 // EAGAIN
	}
	*(*uint32)(unsafe.Pointer(key)) = id
	if dtor != 0 {
		tlsStore.mu.Lock()
		tlsStore.dtors[id] = dtor
		tlsStore.mu.Unlock()
	}
	return 0
}

//go:noinline
func goPthreadKeyDelete(key uintptr) int {
	tlsStore.mu.Lock()
	delete(tlsStore.dtors, uint32(key))
	for _, km := range tlsStore.m {
		delete(km, uint32(key))
	}
	tlsStore.mu.Unlock()
	return 0
}

// pthread_getspecific / pthread_setspecific back the engine's thread-local
// storage. The engine's bundled tcmalloc keeps its per-thread cache behind a
// pthread key, so these must genuinely store and return values; returning zero
// from every get (as an earlier stub did) made the allocator believe no cache
// existed, take a slow path, and abort.
//
// Storage is keyed by the current goroutine's g pointer, obtained from getg().
// A guest thread is a Go goroutine here, and its g pointer is stable for its
// lifetime, so this is a faithful model of per-thread storage: the main thread
// and each created thread get independent slots, which is what a thread cache
// requires.
type tlsTable struct {
	mu sync.Mutex
	m  map[uintptr]map[uint32]uintptr
	// dtors records the destructor registered for each key, indexed by key.
	dtors map[uint32]uintptr
}

var tlsStore = tlsTable{m: map[uintptr]map[uint32]uintptr{}, dtors: map[uint32]uintptr{}}

func tlsGet(g uintptr, key uint32) uintptr {
	tlsStore.mu.Lock()
	defer tlsStore.mu.Unlock()
	if km := tlsStore.m[g]; km != nil {
		return km[key]
	}
	return 0
}

func tlsSet(g uintptr, key uint32, val uintptr) {
	tlsStore.mu.Lock()
	defer tlsStore.mu.Unlock()
	km := tlsStore.m[g]
	if km == nil {
		km = map[uint32]uintptr{}
		tlsStore.m[g] = km
	}
	km[key] = val
}

var gsCallLog atomic.Int32
var gsSetLog atomic.Int32
var deepDumped atomic.Int32

// chainCheckpoint selects getspecific call counts at which the immediate guest
// call chain is dumped, to observe the transition from normal start-up frames
// into whatever recursion develops.
var chainCheckpoint = map[int]bool{
	100: true, 500: true, 1000: true, 2000: true, 5000: true, 10000: true,
	20000: true, 30000: true, 50000: true, 75000: true, 100000: true,
	150000: true, 200000: true, 250000: true, 300000: true,
}

// debugTLS enables per-call tracing of the TLS and guest-stack state used while
// bringing up guest execution on the dedicated stack. Off unless RBXRT_DEBUG_TLS
// is set, because it runs on every pthread_getspecific call.
var debugTLS = os.Getenv("RBXRT_DEBUG_TLS") != ""

// debugMem enables the verbose stack/guest-memory diagnostics used while
// bringing up guest execution. Off unless RBXRT_DEBUG_MEM is set.
var debugMem = os.Getenv("RBXRT_DEBUG_MEM") != ""

// debugStack traces every host->guest entry: which stack it runs on and whether
// the guest stack was already marked in use (which forces the no-switch path).
// Off unless RBXRT_DEBUG_STACK is set.
var debugStack = os.Getenv("RBXRT_DEBUG_STACK") != ""

// debugClock rate-limits a trace of clock_gettime, used to locate a guest
// busy-wait. Off unless RBXRT_DEBUG_CLOCK is set.
var debugClock = os.Getenv("RBXRT_DEBUG_CLOCK") != ""
var clockGetCount uint64

// debugJNI enables a per-call trace of the JNI entry point (capped, so a tight
// loop cannot flood the log). Off unless RBXRT_DEBUG_JNI is set.
var debugJNI = os.Getenv("RBXRT_DEBUG_JNI") != ""

// debugWalk dumps the guest's RBP chain when a fatal guest call (exit/abort)
// reaches us, which is how the exit(1000) site was localised to a repeating
// recursion cycle. Off unless RBXRT_WALK is set: the walk follows raw guest
// pointers, so a chain that is not really frame-pointer linked can fault; it is
// a diagnostic, not a summary report.
var debugWalk = os.Getenv("RBXRT_WALK") != ""

//go:noinline
func goPthreadGetspecific(key uintptr) uintptr {
	v := tlsGet(getg(), uint32(key))
	if !debugTLS {
		return v
	}
	n := int(gsCallLog.Add(1))
	if chainCheckpoint[n] {
		g := getg()
		gs := getGuestStack(g)
		lo := *(*uintptr)(unsafe.Pointer(g))
		hi := *(*uintptr)(unsafe.Pointer(g + 8))
		g0 := *(*uintptr)(unsafe.Pointer(g + stackguardOff))
		g1 := *(*uintptr)(unsafe.Pointer(g + stackguardOff + 8))
		fmt.Fprintf(os.Stderr, "rbxrt: chain n=%d set=%d g=%#x sp=%#x depth=%d stack=[%#x,%#x) guard0=%#x guard1=%#x gstop=%#x guardOff=%d\n",
			n, gsSetLog.Load(), g, currentSP(), gs.depth, lo, hi, g0, g1, gs.top, stackguardOff)
		if n == 50000 || n == 150000 {
			walkGuestFrames(60)
		}
	}
	// The first time any goroutine's guest stack gets deep, capture both the
	// immediate recursion window and the outermost (trigger) frames. Checked on
	// every call because the depth checkpoints above can miss the deep moment.
	if deepDumped.Load() == 0 {
		g := getg()
		if gs := getGuestStack(g); gs != nil && gs.top-currentSP() > 0x400000 {
			if deepDumped.CompareAndSwap(0, 1) {
				fmt.Fprintf(os.Stderr, "rbxrt: DEEP n=%d g=%#x sp=%#x top=%#x depth=%#x\n",
					n, g, currentSP(), gs.top, gs.top-currentSP())
				dumpCtx()
				fmt.Fprintf(os.Stderr, "rbxrt: DEEP window (recursion cycle):\n")
				dumpRawGuestStack(currentSP(), 0x8000, 400)
				fmt.Fprintf(os.Stderr, "rbxrt: DEEP top (trigger chain):\n")
				dumpRawGuestStack(gs.top-0x400, 0x400, 400)
			}
		}
	}
	return v
}

// dumpRawGuestStack prints up to max 8-byte words on the guest stack from sp
// upward that look like they point into the guest image, revealing call chains.
func dumpRawGuestStack(sp uintptr, n, max int) {
	// printed caps output so a fully recursed stack does not flood the log.
	printed := 0
	for i := 0; i < n && printed < max; i += 8 {
		v := *(*uintptr)(unsafe.Pointer(sp + uintptr(i)))
		if guestBias != 0 && v >= guestBias {
			fmt.Fprintf(os.Stderr, "  gueststk[%#x] = %#x (+%#x)\n", sp+uintptr(i), v, v-guestBias)
			printed++
		}
	}
}

// walkGuestFrames walks the guest's RBP chain captured at the most recent host
// thunk on this goroutine, printing the guest call chain up to max frames. It is
// used from fatal paths (see goExit) and is only as trustworthy as the captured
// RBP: a chain that is not frame-pointer linked can take it into unmapped guest
// memory, so callers gate it behind RBXRT_WALK.
func walkGuestFrames(max int) {
	_, rbp := guestCapture()
	for i := 0; i < max && rbp != 0; i++ {
		next := *(*uintptr)(unsafe.Pointer(rbp))
		ret := *(*uintptr)(unsafe.Pointer(rbp + 8))
		fmt.Fprintf(os.Stderr, "  guestframe[%d] rbp=%#x ret=%#x (+%#x)\n", i, rbp, ret, ret-guestBias)
		if next <= rbp {
			break
		}
		rbp = next
	}
}

// dumpCtx decodes the two distinct thread-local objects involved in F
// (0x6d409de): the object that 0x2de3c50 inspects (emutls control 0x72bae40,
// field +0x410, then +0x28) that decides F's branch, and the object whose
// +0x720 slot F calls on the "not registered" branch (emutls control
// 0x72bad08).
func dumpCtx() {
	if guestBias == 0 {
		return
	}
	g := getg()
	arr := tlsGet(g, 1)
	// emutlsObj returns the object pointer __emutls_get_address would return
	// for the control at the given guest virtual address.
	emutlsObj := func(ctrlVaddr uintptr) uintptr {
		ctrl := guestBias + ctrlVaddr
		idx := *(*uintptr)(unsafe.Pointer(ctrl + 0x10))
		if arr == 0 || idx == 0 {
			return 0
		}
		return *(*uintptr)(unsafe.Pointer(arr + 8 + idx*8))
	}
	// The branch object: rcx = *(emutlsObj(0x72bae40)+0x410); test rcx->0x28.
	bo := emutlsObj(0x72bae40)
	fmt.Fprintf(os.Stderr, "rbxrt: BR obj_72bae40=%#x arr=%#x\n", bo, arr)
	if bo != 0 {
		rcx := *(*uintptr)(unsafe.Pointer(bo + 0x410))
		fmt.Fprintf(os.Stderr, "rbxrt: BR obj+0x410=%#x\n", rcx)
		if rcx != 0 {
			fmt.Fprintf(os.Stderr, "rbxrt: BR rcx[0x28]=%#x [0x30]=%#x [0x20]=%#x\n",
				*(*uintptr)(unsafe.Pointer(rcx + 0x28)),
				*(*uintptr)(unsafe.Pointer(rcx + 0x30)),
				*(*uintptr)(unsafe.Pointer(rcx + 0x20)))
		}
	}
	// The error-path object: rcx = *(emutlsObj(0x72bad08)); call *(rcx+0x720).
	eo := emutlsObj(0x72bad08)
	fmt.Fprintf(os.Stderr, "rbxrt: EO obj_72bad08=%#x\n", eo)
	if eo == 0 {
		return
	}
	ctx := *(*uintptr)(unsafe.Pointer(eo))
	fmt.Fprintf(os.Stderr, "rbxrt: EO *obj=%#x\n", ctx)
	if ctx == 0 {
		return
	}
	for off := uintptr(0x700); off < 0x740; off += 8 {
		v := *(*uintptr)(unsafe.Pointer(ctx + off))
		fmt.Fprintf(os.Stderr, "rbxrt: EO ctx[%#04x] = %#x (+%#x)\n", off, v, v-guestBias)
	}
}

//go:noinline
func goPthreadSetspecific(key, val uintptr) int {
	gsSetLog.Add(1)
	tlsSet(getg(), uint32(key), val)
	return 0
}

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
func goPthreadCreate(threadp, attr, start, arg uintptr) int {
	// Guest threads run on Go goroutines. The guest's own TLS (via
	// pthread_getspecific) is keyed by the g pointer, so each created thread
	// gets independent storage, which is what the engine's allocator needs.
	// This is not a full pthread: there is no guest stack or guest %fs TCB, so
	// code that touches %fs directly (rather than through emutls) is not
	// covered. See RunInit's note.
	if debugMem {
		fmt.Fprintf(os.Stderr, "rbxrt: pthread_create start=%s arg=%s\n",
			guestAddr(start), guestAddr(arg))
		if guestBias != 0 && arg >= guestBias {
			fmt.Fprintf(os.Stderr, "  thread arg struct: [0]=%s [8]=%s [0x10]=%s\n",
				guestAddr(ld64(arg)), guestAddr(ld64(arg+8)), guestAddr(ld64(arg+0x10)))
			fmt.Fprintf(os.Stderr, "  thread cbs: [7307ef8]=%s [7307f00]=%s\n",
				guestAddr(ld64(guestBias+0x7307ef8)), guestAddr(ld64(guestBias+0x7307f00)))
		}
	}
	t := &guestThread{done: make(chan struct{})}
	p := uintptr(unsafe.Pointer(t))
	threadMu.Lock()
	threads[p] = t
	threadMu.Unlock()
	guestThreads.Add(1)
	if threadp != 0 {
		*(*uintptr)(unsafe.Pointer(threadp)) = p
	}
	go func() {
		defer guestThreads.Add(-1)
		g := getg()
		t.ret = uintptr(CallGuestInt3(start, arg, 0, 0))
		// The goroutine is about to end, and the Go runtime will recycle its g
		// struct for a later goroutine. Drop everything keyed by this g now, or
		// the next guest thread inherits this one's emutls array -- including
		// the engine's per-thread task state and fatal-handler refcounts.
		releaseThreadLocal(g)
		threadMu.Lock()
		detached := t.detached
		if detached {
			delete(threads, p)
		}
		threadMu.Unlock()
		close(t.done)
	}()
	return 0
}

// releaseThreadLocal tears down the per-thread state keyed by a finished
// goroutine's g pointer. The g pointer identifies a guest thread only while
// that goroutine lives; afterwards the Go runtime reuses the struct, so any
// state left behind would be silently inherited by an unrelated guest thread.
func releaseThreadLocal(g uintptr) {
	tlsStore.mu.Lock()
	km := tlsStore.m[g]
	dtors := tlsStore.dtors
	delete(tlsStore.m, g)
	tlsStore.mu.Unlock()

	// Run registered TLS destructors, as pthread does when a thread exits, with
	// a few passes in case a destructor sets another key.
	for pass := 0; pass < 4 && km != nil; pass++ {
		ran := false
		for key, dtor := range dtors {
			if v, ok := km[key]; ok && v != 0 {
				delete(km, key)
				CallGuestVoid1(dtor, v)
				ran = true
			}
		}
		if !ran {
			break
		}
	}

	// Drop the thread's JNIEnv slot so the reused g struct cannot inherit a
	// stale env (and the per-thread table the engine cached in *env).
	releaseEnvSlot(g)

	gsMu.Lock()
	gs := gsMap[g]
	delete(gsMap, g)
	gsMu.Unlock()
	gs.release()
}

// guestThread is the host-side handle behind a pthread_t the guest sees. It is
// returned to the guest as an opaque pointer.
type guestThread struct {
	done     chan struct{}
	ret      uintptr
	detached bool
}

var (
	threadMu sync.Mutex
	threads  = map[uintptr]*guestThread{}

	// guestThreads counts live guest worker goroutines. codeArena consults it
	// to decide whether reopening the code arena for writing is safe (no guest
	// code can be executing) or a fresh slab must be mapped instead.
	guestThreads atomic.Int64
)

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
