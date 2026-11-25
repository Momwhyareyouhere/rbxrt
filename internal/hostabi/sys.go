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

// legacyDiagOffsets enables the few diagnostics that read fixed addresses of
// the Roblox 2.734.0.917 image (dumpCtx and the thread-callback dump). On any
// other build those addresses hold unrelated data, so they stay off unless the
// build profile says the image is that build.
var legacyDiagOffsets bool

// SetLegacyDiagOffsets turns on the 2.734.0.917-specific diagnostics.
func SetLegacyDiagOffsets(on bool) { legacyDiagOffsets = on }

// SetGuestBias records the primary guest image's load bias for diagnostics.
// The loader calls this before any guest code runs.
func SetGuestBias(b uintptr) { guestBias = b }

// PatchGuest overwrites bytes in the guest image. It exists only for
// instrumentation experiments (the text segment is mapped read-only); it
// temporarily makes the containing pages writable and restores RX afterwards.
//
// Prefer PatchGuestChecked: PatchGuest writes unconditionally, so an offset
// taken from a different engine build silently corrupts unrelated code.
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
//
//go:nosplit
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
//go:nosplit
func goTrap(namePtr uintptr) {
	// This is entered on the guest stack; formatting the diagnostic with Go
	// would attempt a stack split and hide the actual missing symbol. Write the
	// symbol directly, then terminate through the kernel.
	if namePtr != 0 {
		n := uintptr(0)
		for n < 4096 && *(*byte)(unsafe.Pointer(namePtr + n)) != 0 {
			n++
		}
		rawSysNoEnter(1, 2, namePtr, n)
		nl := byte('\n')
		rawSysNoEnter(1, 2, uintptr(unsafe.Pointer(&nl)), 1)
	}
	// exit_group, not exit: SYS_exit (60) ends only the calling thread, which
	// left the rest of the engine waiting forever on work that thread owned.
	rawSysNoEnter(231, 70, 0, 0)
	for {
	}
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
	writeStderrRaw("rbxrt: [" + tag + "] " + msg + "\n")
}

// writeStderrRaw writes s to fd 2 with a raw syscall. Host functions called
// from guest code run on the guest stack, where a Go syscall (os.Stderr, fmt.
// Fprint to a file) aborts with "entersyscall inconsistent sp"; diagnostics on
// those paths must come through here.
func writeStderrRaw(s string) {
	line := []byte(s)
	for len(line) != 0 {
		n := rawSysNoEnter(1, 2, uintptr(unsafe.Pointer(&line[0])), uintptr(len(line)))
		if int64(n) <= 0 {
			break
		}
		line = line[int(n):]
	}
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
	// SYS_GETTID, no arguments
	return rawSysNoEnter(syscall.SYS_GETTID, 0, 0, 0)
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
//go:nosplit
func goSysconf(name uintptr) int {
	switch int(name) {
	case 0x06: // _SC_CLK_TCK
		return 100
	case 0x0b: // _SC_OPEN_MAX
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
//go:nosplit
func goClockGettime(clockid, tp uintptr) int {
	if tp == 0 {
		return -14 // EFAULT
	}
	// Linux x86_64 SYS_clock_gettime. Stay entirely outside the Go runtime:
	// this callback runs on a guest-provided stack.
	r := rawSysNoEnter(228, clockid, tp, 0)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return int(r)
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
//go:nosplit
func goMmap(addr, length, prot, flags, fd, off uintptr) uintptr {
	p := rawSysNoEnter6(9, addr, length, prot, flags, fd, off)
	if int64(p) < 0 {
		setErrno(int32(-int64(p)))
		return ^uintptr(0) // MAP_FAILED
	}
	return p
}

//go:noinline
//go:nosplit
func goMunmap(addr, length uintptr) int {
	if r := rawSysNoEnter6(11, addr, length, 0, 0, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
//go:nosplit
func goMprotect(addr, length, prot uintptr) int {
	if r := rawSysNoEnter6(10, addr, length, prot, 0, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
//go:nosplit
func goMadvise(addr, length, advice uintptr) int {
	r := rawSysNoEnter(28, addr, length, advice)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

// --- file descriptors ----------------------------------------------------

//go:noinline
//go:nosplit
func goOpen(path, flags, mode uintptr) int {
	r := sysRet(rawSysNoEnter(2, path, flags, mode))
	if debugFS {
		traceFS("open", path, r)
	}
	return r
}

var debugAssetFiles = os.Getenv("RBXRT_DEBUG_ASSET_FILES") != ""

func debugAssetFileOpen(op string, path, result uintptr) {
	if !debugAssetFiles {
		return
	}
	p := cstr(path)
	if !strings.Contains(p, "UniversalApp") && !strings.Contains(p, "configs/") && !strings.Contains(p, "ExtraContent") {
		return
	}
	b := []byte(fmt.Sprintf("rbxrt: %s %q -> %d\n", op, p, int64(result)))
	for len(b) > 0 {
		n := rawSysNoEnter(1, 2, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
		if int64(n) <= 0 {
			return
		}
		b = b[int(n):]
	}
}

//go:noinline
//go:nosplit
func goClose(fd uintptr) int {
	r := sysRet(rawSysNoEnter(3, fd, 0, 0))
	if isNetFD(fd) {
		traceNet("close", fd, r, "")
		netFDs.Delete(fd)
	}
	return r
}

//go:noinline
func goRead(fd, buf, n uintptr) int {
	r := rawSysNoEnter(0, fd, buf, n)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		tracePipeIO("read", fd, buf, n, -1)
		if debugNet && isNetFD(fd) {
			traceNet("read", fd, -1, fmt.Sprintf(", n=%d", n))
		}
		return -1
	}
	tracePipeIO("read", fd, buf, n, int(r))
	if debugNet && isNetFD(fd) {
		traceNet("read", fd, int(r), fmt.Sprintf(", n=%d", n))
	}
	return int(r)
}

//go:noinline
func goWrite(fd, buf, n uintptr) int {
	r := rawSysNoEnter(1, fd, buf, n)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		tracePipeIO("write", fd, buf, n, -1)
		if debugNet && isNetFD(fd) {
			traceNet("write", fd, -1, fmt.Sprintf(", n=%d", n))
		}
		return -1
	}
	tracePipeIO("write", fd, buf, n, int(r))
	if debugNet && isNetFD(fd) {
		traceNet("write", fd, int(r), fmt.Sprintf(", n=%d", n))
	}
	return int(r)
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
	r := rawSysNoEnter6(17, fd, buf, n, off, 0, 0)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return int(r)
}

//go:noinline
func goPwrite(fd, buf, n, off uintptr) int {
	r := rawSysNoEnter6(18, fd, buf, n, off, 0, 0)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return int(r)
}

//go:noinline
func goLseek(fd, off, whence uintptr) int64 {
	r := rawSysNoEnter(8, fd, off, whence)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return int64(r)
}

//go:noinline
func goAccess(path, mode uintptr) int {
	r := sysRet(rawSysNoEnter(21, path, mode, 0))
	if debugFS {
		traceFS("access", path, r)
	}
	return r
}

//go:noinline
func goFcntl(fd, cmd, arg uintptr) int {
	r := rawSysNoEnter(72, fd, cmd, arg)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return int(r)
}

//go:noinline
func goGetcwd(buf, size uintptr) uintptr {
	r := rawSysNoEnter(79, buf, size, 0)
	if int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return 0
	}
	return buf
}

//go:noinline
func goMkdir(path, mode uintptr) int {
	if r := rawSysNoEnter(83, path, mode, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goRmdir(path uintptr) int {
	if r := rawSysNoEnter(84, path, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

//go:noinline
func goUnlink(path uintptr) int {
	if r := rawSysNoEnter(87, path, 0, 0); int64(r) < 0 {
		setErrno(int32(-int64(r)))
		return -1
	}
	return 0
}

// --- pthread mutexes -----------------------------------------------------
//
// Guest threads are real OS threads (goroutines), and the engine and its
// third-party libraries (FMOD in particular) rely on recursive mutexes, so the
// mutex follows bionic's semantics inside bionic's 40-byte pthread_mutex_t:
//
//	+0  uint32  bits 14-15: type (0 normal, 1 recursive, 2 errorcheck), as
//	            bionic encodes it, so its static initializers work unchanged
//	            (PTHREAD_RECURSIVE_MUTEX_INITIALIZER_NP is 0x4000);
//	            bits 0-1: lock state 0 free, 1 locked, 2 locked with waiters
//	+8  uint64  owner (the guest thread's goroutine), for recursive/errorcheck
//	+16 uint32  recursion depth beyond the first lock
//
// The lock word is a futex (Drepper's three-state mutex), so waiters sleep in
// the kernel instead of spinning. pthread_mutexattr_t is bionic's long; it
// holds the type.

const (
	mutexStateMask = 3
	mutexTypeShift = 14
	mutexTypeMask  = 3 << mutexTypeShift

	mutexNormal     = 0
	mutexRecursive  = 1
	mutexErrorcheck = 2

	errEPERM   = 1
	errEBUSY   = 16
	errEINVAL  = 22
	errEDEADLK = 35
)

func mutexWord(m uintptr) *uint32   { return (*uint32)(unsafe.Pointer(m)) }
func mutexOwner(m uintptr) *uintptr { return (*uintptr)(unsafe.Pointer(m + 8)) }
func mutexCount(m uintptr) *uint32  { return (*uint32)(unsafe.Pointer(m + 16)) }
func mutexType(m uintptr) uint32 {
	return (atomic.LoadUint32(mutexWord(m)) & mutexTypeMask) >> mutexTypeShift
}
func mutexSetState(m uintptr, st uint32) (old uint32) {
	p := mutexWord(m)
	for {
		v := atomic.LoadUint32(p)
		if atomic.CompareAndSwapUint32(p, v, v&^mutexStateMask|st) {
			return v & mutexStateMask
		}
	}
}

// mutexAcquire takes the lock word, sleeping on the futex while it is held.
func mutexAcquire(m uintptr) {
	p := mutexWord(m)
	tb := atomic.LoadUint32(p) &^ mutexStateMask
	if atomic.CompareAndSwapUint32(p, tb, tb|1) {
		return
	}
	for mutexSetState(m, 2) != 0 {
		futexDo(m, futexOpWait|futexPrivate, uintptr(tb|2), 0)
	}
}

// mutexTryAcquire takes the lock word only if it is free.
func mutexTryAcquire(m uintptr) bool {
	p := mutexWord(m)
	tb := atomic.LoadUint32(p) &^ mutexStateMask
	return atomic.CompareAndSwapUint32(p, tb, tb|1)
}

// mutexRelease frees the lock word and wakes one waiter if there are any.
func mutexRelease(m uintptr) {
	if mutexSetState(m, 0) == 2 {
		futexDo(m, futexOpWake|futexPrivate, 1, 0)
	}
}

//go:noinline
func goPthreadMutexInit(m, attr uintptr) int {
	typ := uint32(mutexNormal)
	if attr != 0 {
		typ = uint32(*(*int64)(unsafe.Pointer(attr))) & 3
	}
	*mutexWord(m) = typ << mutexTypeShift
	*mutexOwner(m) = 0
	*mutexCount(m) = 0
	return 0
}

//go:noinline
func goPthreadMutexLock(m uintptr) int {
	if t := mutexType(m); t != mutexNormal {
		self := getg()
		if *mutexOwner(m) == self {
			if t == mutexErrorcheck {
				return errEDEADLK
			}
			*mutexCount(m)++
			return 0
		}
		mutexAcquire(m)
		*mutexOwner(m) = self
		*mutexCount(m) = 0
		return 0
	}
	mutexAcquire(m)
	return 0
}

//go:noinline
func goPthreadMutexTrylock(m uintptr) int {
	if t := mutexType(m); t != mutexNormal {
		self := getg()
		if *mutexOwner(m) == self {
			if t == mutexErrorcheck {
				return errEBUSY
			}
			*mutexCount(m)++
			return 0
		}
		if !mutexTryAcquire(m) {
			return errEBUSY
		}
		*mutexOwner(m) = self
		*mutexCount(m) = 0
		return 0
	}
	if mutexTryAcquire(m) {
		return 0
	}
	return errEBUSY
}

//go:noinline
func goPthreadMutexUnlock(m uintptr) int {
	if t := mutexType(m); t != mutexNormal {
		if *mutexOwner(m) != getg() {
			return errEPERM
		}
		if *mutexCount(m) > 0 {
			*mutexCount(m)--
			return 0
		}
		*mutexOwner(m) = 0
	}
	mutexRelease(m)
	return 0
}

// mutexUnlockForWait fully releases m for a condition wait, whatever its
// recursion depth, and returns the depth so mutexRelockAfterWait can restore
// it (POSIX: the wait releases the mutex, not one level of it).
func mutexUnlockForWait(m uintptr) uint32 {
	var depth uint32
	if mutexType(m) != mutexNormal {
		depth = *mutexCount(m)
		*mutexCount(m) = 0
		*mutexOwner(m) = 0
	}
	mutexRelease(m)
	return depth
}

func mutexRelockAfterWait(m uintptr, depth uint32) {
	mutexAcquire(m)
	if mutexType(m) != mutexNormal {
		*mutexOwner(m) = getg()
		*mutexCount(m) = depth
	}
}

//go:noinline
func goPthreadMutexDestroy(m uintptr) int { return 0 }

//go:noinline
func goPthreadMutexattrInit(a uintptr) int {
	*(*int64)(unsafe.Pointer(a)) = mutexNormal
	return 0
}

//go:noinline
func goPthreadMutexattrDestroy(a uintptr) int { return 0 }

//go:noinline
func goPthreadMutexattrSettype(a, t uintptr) int {
	if t > mutexErrorcheck {
		return errEINVAL
	}
	*(*int64)(unsafe.Pointer(a)) = int64(t)
	return 0
}

//go:noinline
//go:nosplit
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
//go:nosplit
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
			// This callback runs on the guest stack; Gosched may grow it while
			// waiting. The initializer is short, so spin without entering the
			// Go scheduler.
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
		tlsStore.dtorMu.Lock()
		tlsStore.dtors[id] = dtor
		tlsStore.dtorMu.Unlock()
	}
	return 0
}

//go:noinline
func goPthreadKeyDelete(key uintptr) int {
	tlsStore.dtorMu.Lock()
	delete(tlsStore.dtors, uint32(key))
	tlsStore.dtorMu.Unlock()
	for i := range tlsStore.slots {
		s := &tlsStore.slots[i]
		if atomic.LoadUint32(&s.state) == 2 && s.key == uint32(key) {
			atomic.StoreUintptr(&s.value, 0)
		}
	}
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
	dtorMu sync.Mutex
	slots  [1 << 15]tlsSlot
	// dtors records the destructor registered for each key, indexed by key.
	dtors map[uint32]uintptr
}

type tlsSlot struct {
	state uint32 // 0 empty, 1 being initialized, 2 published
	key   uint32
	g     uintptr
	value uintptr
}

var tlsStore = tlsTable{dtors: map[uint32]uintptr{}}

func tlsGet(g uintptr, key uint32) uintptr {
	return tlsGetLocked(g, key)
}

//go:nosplit
func tlsGetLocked(g uintptr, key uint32) uintptr {
	idx := int((g>>4 ^ uintptr(key)*0x9e3779b97f4a7c15) & uintptr(len(tlsStore.slots)-1))
	for n := 0; n < len(tlsStore.slots); n++ {
		s := &tlsStore.slots[(idx+n)&(len(tlsStore.slots)-1)]
		state := atomic.LoadUint32(&s.state)
		if state == 0 {
			return 0
		}
		if state == 2 && atomic.LoadUintptr(&s.g) == g && s.key == key {
			return atomic.LoadUintptr(&s.value)
		}
	}
	return 0
}

func tlsSet(g uintptr, key uint32, val uintptr) {
	tlsSetLocked(g, key, val)
}

//go:nosplit
func tlsSetLocked(g uintptr, key uint32, val uintptr) {
	idx := int((g>>4 ^ uintptr(key)*0x9e3779b97f4a7c15) & uintptr(len(tlsStore.slots)-1))
	for n := 0; n < len(tlsStore.slots); n++ {
		s := &tlsStore.slots[(idx+n)&(len(tlsStore.slots)-1)]
		state := atomic.LoadUint32(&s.state)
		if state == 2 && atomic.LoadUintptr(&s.g) == g && s.key == key {
			atomic.StoreUintptr(&s.value, val)
			return
		}
		if state == 0 && atomic.CompareAndSwapUint32(&s.state, 0, 1) {
			s.key = key
			atomic.StoreUintptr(&s.g, g)
			atomic.StoreUintptr(&s.value, val)
			atomic.StoreUint32(&s.state, 2)
			return
		}
	}
}

var gsCallLog atomic.Int32
var gsSetLog atomic.Int32
var deepDumped atomic.Int32
var tlsTraceBudget atomic.Int32

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
//go:nosplit
func goPthreadGetspecific(key uintptr) uintptr {
	g := getg()
	v := tlsGetLocked(g, uint32(key))
	if !debugTLS {
		return v
	}
	return tracePthreadGetspecific(g, key, v)
}

// tracePthreadGetspecific is separate from the hot shim so its large diagnostic
// frame is not entered while a guest callback is executing on the guest stack.
// It is the only caller of the TLS diagnostics; the hot path above takes none
// of it.
//
//go:noinline
func tracePthreadGetspecific(g, key, v uintptr) uintptr {
	n := int(gsCallLog.Add(1))
	if takeTLSTraceSlot() {
		pc, _ := guestCapture()
		fmt.Fprintf(os.Stderr, "rbxrt: tls get n=%d key=%#x -> %#x guest-pc=%s\n",
			n, key, v, guestAddr(pc))
	}
	if chainCheckpoint[n] {
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
		writeStderrRaw(fmt.Sprintf("  guestframe[%d] rbp=%#x ret=%#x (+%#x)\n", i, rbp, ret, ret-guestBias))
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
	if guestBias == 0 || !legacyDiagOffsets {
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
//go:nosplit
func goPthreadSetspecific(key, val uintptr) int {
	n := int(gsSetLog.Add(1))
	if debugTLS {
		g := getg()
		tlsSetLocked(g, uint32(key), val)
		if takeTLSTraceSlot() {
			pc, _ := guestCapture()
			fmt.Fprintf(os.Stderr, "rbxrt: tls set n=%d key=%#x value=%#x guest-pc=%s\n",
				n, key, val, guestAddr(pc))
		}
		return 0
	}
	tlsSetLocked(getg(), uint32(key), val)
	return 0
}

// TraceTLSCalls enables a bounded trace of pthread-specific storage operations.
// The launch driver calls it immediately before a boot stage so constructor
// traffic does not consume the diagnostic budget.
func TraceTLSCalls(n int) {
	tlsTraceBudget.Store(int32(n))
}

func takeTLSTraceSlot() bool {
	for n := tlsTraceBudget.Load(); n > 0; n = tlsTraceBudget.Load() {
		if tlsTraceBudget.CompareAndSwap(n, n-1) {
			return true
		}
	}
	return false
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
			if legacyDiagOffsets {
				fmt.Fprintf(os.Stderr, "  thread cbs: [7307ef8]=%s [7307f00]=%s\n",
					guestAddr(ld64(guestBias+0x7307ef8)), guestAddr(ld64(guestBias+0x7307f00)))
			}
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
		// A guest thread is an OS thread for its whole life, as a pthread is.
		// The host libraries it calls keep per-OS-thread state - most visibly
		// Mesa's current EGL context: eglMakeCurrent on one OS thread and
		// glGetString on another returns NULL - and Go would otherwise move
		// the goroutine between OS threads at any scheduling point. The lock is
		// deliberately never released, so when this goroutine ends Go retires
		// the OS thread with it (like pthread_exit) instead of reusing a thread
		// that still has the engine's native state bound to it.
		runtime.LockOSThread()
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
	km := make(map[uint32]uintptr)
	for i := range tlsStore.slots {
		s := &tlsStore.slots[i]
		if atomic.LoadUint32(&s.state) == 2 && atomic.LoadUintptr(&s.g) == g {
			if v := atomic.SwapUintptr(&s.value, 0); v != 0 {
				km[s.key] = v
			}
		}
	}
	tlsStore.dtorMu.Lock()
	dtors := make(map[uint32]uintptr, len(tlsStore.dtors))
	for key, dtor := range tlsStore.dtors {
		dtors[key] = dtor
	}
	tlsStore.dtorMu.Unlock()

	// Run registered TLS destructors, as pthread does when a thread exits, with
	// a few passes in case a destructor sets another key.
	for pass := 0; pass < 4 && len(km) > 0; pass++ {
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

// GuestBytes returns a copy of n bytes of guest memory at addr.
func GuestBytes(addr uintptr, n int) []byte {
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(addr)), n))
	return out
}

// PatchGuestChecked is PatchGuest guarded by the instruction it replaces:
// check receives the current bytes at addr (len(b) of them) and returns an
// error when they are not what the patch was written against. Nothing is
// written in that case, so an offset from another engine build fails loudly
// instead of corrupting unrelated code.
func PatchGuestChecked(addr uintptr, b []byte, check func(cur []byte) error) error {
	if check != nil {
		cur := GuestBytes(addr, len(b))
		if err := check(cur); err != nil {
			return fmt.Errorf("refusing patch at %s (found % x): %w", guestAddr(addr), cur, err)
		}
	}
	return PatchGuest(addr, b)
}
