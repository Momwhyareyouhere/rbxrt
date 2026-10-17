package hostabi

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// This file implements the process, scheduling, signal, directory and assorted
// system-call wrappers of the bionic-compatible host. Where bionic is a thin
// wrapper over a Linux syscall, so is the host: the guest's own buffer is
// handed straight to the kernel, which is exactly what bionic does, so no
// structure translation is needed.

// Non-portable syscall numbers used below (Linux x86-64). They are spelled out
// rather than taken from syscall.SYS_* so the intent is auditable even on
// toolchains that omit a constant.
const (
	nSysReadv          = 19
	nSysWritev         = 20
	nSysSelect         = 23
	nSysMsync          = 26
	nSysPoll           = 7
	nSysUname          = 63
	nSysFork           = 57
	nSysExecve         = 59
	nSysWait4          = 61
	nSysFsync          = 74
	nSysFdatasync      = 75
	nSysPtrace         = 101
	nSysGetpriority    = 140
	nSysSetpriority    = 141
	nSysMlock          = 149
	nSysMunlock        = 150
	nSysMlockall       = 151
	nSysMunlockall     = 152
	nSysPrctl          = 157
	nSysSync           = 162
	nSysFallocate      = 285
	nSysEpollCreate    = 213
	nSysEpollCtl       = 233
	nSysEpollWait      = 232
	nSysEpollCreate1   = 291
	nSysTimerfdCreate  = 283
	nSysTimerfdSettime = 286
	nSysEventfd2       = 290
	nSysGetcpu         = 309
	nSysSysinfo        = 99
	nSysPipe           = 22
	nSysPipe2          = 293
	nSysIoctl          = 16
	nSysGetrandom      = 318
)

// rawSys performs a Linux syscall with up to three arguments and returns the
// raw result. The Go errno is discarded: bionic's wrappers return the raw value
// too and let the caller consult errno via a later call.
func rawSys(trap, a, b, c uintptr) uintptr {
	r, _, _ := syscall.Syscall(trap, a, b, c)
	return r
}

func rawSys6(trap, a, b, c, d, e, f uintptr) uintptr {
	r, _, _ := syscall.Syscall6(trap, a, b, c, d, e, f)
	return r
}

// goSyscall implements bionic's syscall(long number, ...). The host thunk
// captures up to six integer arguments, i.e. the number plus five syscall
// arguments, which covers everything the engine issues. RawSyscall6 returns the
// raw kernel result, so a failure surfaces as -errno exactly as bionic expects
// (syscall.Syscall6 would instead collapse it to -1 and stash errno in the Go
// runtime, which the guest would never see).
//
//go:noinline
func goSyscall(number, a1, a2, a3, a4, a5 uintptr) uintptr {
	// bionic's syscall() is variadic. Our thunk forwards the six integer
	// argument registers, but a call with a seventh C argument (futex's
	// trailing val3, the bitset for FUTEX_WAIT_BITSET/FUTEX_WAKE_BITSET) passes
	// it on the stack; without it the kernel rejects the wait and the caller
	// spins. Recover the argument from the guest SP recorded at thunk entry.
	var a6 uintptr
	if sp := guestCapturedRSP(); sp != 0 {
		a6 = *(*uintptr)(unsafe.Pointer(sp + 8))
	}
	r, _, err := syscall.RawSyscall6(number, a1, a2, a3, a4, a5, a6)
	if debugSys && number == 202 { // futex
		n := atomic.AddUint64(&futexCallCount, 1)
		if n <= 8 || n%1000000 == 0 {
			var to string
			if a4 != 0 {
				to = fmt.Sprintf("ts={%d,%d}", *(*int64)(unsafe.Pointer(a4)), *(*int64)(unsafe.Pointer(a4 + 8)))
			}
			fmt.Fprintf(os.Stderr, "rbxrt: futex uaddr=%#x op=%#x val=%#x to=%#x %s val3=%#x r=%d err=%v nowUs=%d n=%d\n",
				a1, a2, a3, a4, to, a6, int64(r), err, hostMonotonicNowNanos(1)/1000, n)
		}
	}
	return r
}

var debugSys = os.Getenv("RBXRT_DEBUG_SYS") != ""
var futexCallCount uint64

func st64(dst uintptr, v uint64) { *(*uint64)(unsafe.Pointer(dst)) = v }
func st32(dst uintptr, v uint32) { *(*uint32)(unsafe.Pointer(dst)) = v }
func st16(dst uintptr, v uint16) { *(*uint16)(unsafe.Pointer(dst)) = v }
func st8(dst uintptr, v byte)    { *(*byte)(unsafe.Pointer(dst)) = v }

func putStr(dst uintptr, s string) {
	for i := 0; i < len(s); i++ {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = s[i]
	}
}

func putStrZ(dst uintptr, s string) {
	putStr(dst, s)
	*(*byte)(unsafe.Pointer(dst + uintptr(len(s)))) = 0
}

// --- system -----------------------------------------------------------------

//go:noinline
func goSysinfo(info uintptr) int {
	return int(int64(rawSys(nSysSysinfo, info, 0, 0)))
}

//go:noinline
func goUname(buf uintptr) int {
	return int(int64(rawSys(nSysUname, buf, 0, 0)))
}

// system(NULL) reports whether a shell is available; otherwise the command is
// run through /bin/sh, exactly as bionic delegates to /system/bin/sh.
//
//go:noinline
func goSystem(cmd uintptr) int {
	if cmd == 0 {
		if _, err := os.Stat("/bin/sh"); err == nil {
			return 1
		}
		return 0
	}
	c := exec.Command("/bin/sh", "-c", cstr(cmd))
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return int(ee.ExitCode()) << 8
		}
		return -1
	}
	return 0
}

// __system_property_get always reports the property as empty, which is how a
// property that is not set behaves.
//
//go:noinline
func goSystemPropertyGet(name, value uintptr) int {
	if value != 0 {
		*(*byte)(unsafe.Pointer(value)) = 0
	}
	return 0
}

// --- process ----------------------------------------------------------------

//go:noinline
func goWaitpid(pid, status, options uintptr) int {
	r := rawSys6(nSysWait4, pid, status, options, 0, 0, 0)
	return int(int64(r))
}

func goFork() int {
	r := rawSys(nSysFork, 0, 0, 0)
	return int(int64(r))
}

func goExecve(path, argv, envp uintptr) int {
	r := rawSys(nSysExecve, path, argv, envp)
	return int(int64(r))
}

func goExecv(path, argv uintptr) int {
	// execv uses the caller's environ.
	r := rawSys(nSysExecve, path, argv, environVar)
	return int(int64(r))
}

// --- scheduling -------------------------------------------------------------

// goSchedGetcpu is bionic's `int sched_getcpu(void)`: a guest call passes no
// arguments, so rdi/rsi/rdx are whatever the caller happened to be holding. The
// engine's own allocator (0x2de6ef0) calls this with its allocator object in
// rdi and the request size in rsi; forwarding those as getcpu()'s output
// pointers made the kernel store the running CPU into the caller's object and
// reported a bogus CPU. The output buffer must therefore be ours.
//
//go:noinline
func goSchedGetcpu(_, _, _ uintptr) int {
	var cpu int32
	if r := rawSys(nSysGetcpu, uintptr(unsafe.Pointer(&cpu)), 0, 0); r != 0 {
		return int(int64(r))
	}
	return int(cpu)
}

//go:noinline
func goSchedGetPriorityMax(policy uintptr) int { return 0 }

//go:noinline
func goSchedGetPriorityMin(policy uintptr) int { return 0 }

//go:noinline
func goSchedGetscheduler(pid uintptr) int { return 0 }

//go:noinline
func goSchedSetscheduler(pid, policy, param uintptr) int { return 0 }

//go:noinline
func goSchedGetparam(pid, param uintptr) int {
	if param != 0 {
		st32(param, 0)
	}
	return 0
}

//go:noinline
func goGetpriority(which, who uintptr) int {
	return int(int64(rawSys(nSysGetpriority, which, who, 0)))
}

//go:noinline
func goSetpriority(which, who, prio uintptr) int {
	return int(int64(rawSys(nSysSetpriority, which, who, prio)))
}

//go:noinline
func goPrctl(op, a2, a3, a4, a5 uintptr) int {
	return int(int64(rawSys6(nSysPrctl, op, a2, a3, a4, a5, 0)))
}

// ptrace is not supportable: a guest debugging itself through us is out of
// scope. Report failure rather than pretending.
//
//go:noinline
func goPtrace(request, pid, addr, data uintptr) int {
	return -1
}

// --- memory advise ----------------------------------------------------------

//go:noinline
func goMlock(addr, length uintptr) int {
	return int(int64(rawSys(nSysMlock, addr, length, 0)))
}

//go:noinline
func goMunlock(addr, length uintptr) int {
	return int(int64(rawSys(nSysMunlock, addr, length, 0)))
}

//go:noinline
func goMsync(addr, length, flags uintptr) int {
	return int(int64(rawSys(nSysMsync, addr, length, flags)))
}

func goFsync(fd uintptr) int {
	return int(int64(rawSys(nSysFsync, fd, 0, 0)))
}

func goFdatasync(fd uintptr) int {
	return int(int64(rawSys(nSysFdatasync, fd, 0, 0)))
}

func goSync() {
	rawSys(nSysSync, 0, 0, 0)
}

// posix_fallocate returns an errno value (not -1) on failure, like bionic.
//
//go:noinline
func goPosixFallocate(fd, offset, length uintptr) int {
	r := int64(rawSys6(nSysFallocate, fd, 0, offset, length, 0, 0))
	if r < 0 {
		return int(-r)
	}
	return 0
}

// mallinfo is deprecated in bionic and returns an all-zero struct. Zeroing the
// storage matches that.
//
//go:noinline
func goMallinfo(buf uintptr) uintptr {
	for i := uintptr(0); i < 80; i += 8 {
		st64(buf+i, 0)
	}
	return buf
}

// --- pipes / vectors --------------------------------------------------------

//go:noinline
func goPipe(fds uintptr) int {
	r := int(int64(rawSys(nSysPipe, fds, 0, 0)))
	if debugSys && r == 0 && fds != 0 {
		fmt.Fprintf(os.Stderr, "rbxrt: pipe -> [%d,%d]\n",
			int(*(*int32)(unsafe.Pointer(fds))), int(*(*int32)(unsafe.Pointer(fds + 4))))
	}
	return r
}

//go:noinline
func goPipe2(fds, flags uintptr) int {
	r := int(int64(rawSys(nSysPipe2, fds, flags, 0)))
	if debugSys && r == 0 && fds != 0 {
		fmt.Fprintf(os.Stderr, "rbxrt: pipe2 flags=%#x -> [%d,%d]\n", flags,
			int(*(*int32)(unsafe.Pointer(fds))), int(*(*int32)(unsafe.Pointer(fds + 4))))
	}
	return r
}

//go:noinline
func goWritev(fd, iov, count uintptr) int {
	return int(int64(rawSys(nSysWritev, fd, iov, count)))
}

//go:noinline
func goReadv(fd, iov, count uintptr) int {
	return int(int64(rawSys(nSysReadv, fd, iov, count)))
}

//go:noinline
func goIoctl(fd, req, arg uintptr) int {
	return int(int64(rawSys(nSysIoctl, fd, req, arg)))
}

// --- random -----------------------------------------------------------------

//go:noinline
func goArc4randomBuf(buf, n uintptr) {
	if buf == 0 || n == 0 {
		return
	}
	rand.Read(unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(n)))
}

// --- filesystem helpers -----------------------------------------------------

// realpath resolves path and copies the result into resolved (which must not be
// NULL; a NULL resolved would require a malloc the host does not provide).
//
//go:noinline
func goRealpath(path, resolved uintptr) uintptr {
	if resolved == 0 {
		return 0
	}
	p := cstr(path)
	abs, err := filepath.EvalSymlinks(p)
	if err != nil {
		// Fall back to a lexical absolute path when the file does not exist,
		// which is what realpath(3) requires (it need not succeed).
		if a, aerr := filepath.Abs(p); aerr == nil {
			abs = filepath.Clean(a)
		} else {
			return 0
		}
	}
	putStrZ(resolved, abs)
	return resolved
}

//go:noinline
func goReadlinkChk(path, buf, size, buflen uintptr) uintptr {
	if size > buflen {
		fortifyFail("__readlink_chk")
	}
	return goReadlink(path, buf, size)
}

// statvfs converts the kernel statfs result into bionic's struct statvfs.
//
//go:noinline
func goStatvfs(path, buf uintptr) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cstr(path), &st); err != nil {
		return -1
	}
	st64(buf+0, uint64(st.Bsize))
	st64(buf+8, uint64(st.Frsize))
	st64(buf+16, st.Blocks)
	st64(buf+24, st.Bfree)
	st64(buf+32, st.Bavail)
	st64(buf+40, st.Files)
	st64(buf+48, st.Ffree)
	st64(buf+56, st.Ffree) // f_favail: Linux has no separate count
	st64(buf+64, uint64(uint32(st.Fsid.X__val[0])))
	st64(buf+72, uint64(st.Flags))
	st64(buf+80, uint64(st.Namelen))
	for i := uintptr(88); i < 112; i += 4 {
		st32(buf+i, 0)
	}
	return 0
}

//go:noinline
func goUtimes(path, times uintptr) int {
	var tv [2]syscall.Timeval
	if times != 0 {
		tv[0].Sec = int64(*(*int64)(unsafe.Pointer(times)))
		tv[0].Usec = int64(*(*int64)(unsafe.Pointer(times + 8)))
		tv[1].Sec = int64(*(*int64)(unsafe.Pointer(times + 16)))
		tv[1].Usec = int64(*(*int64)(unsafe.Pointer(times + 24)))
	}
	if err := syscall.Utimes(cstr(path), tv[:]); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goUtime(path, times uintptr) int {
	now := time.Now()
	var tv [2]syscall.Timeval
	if times != 0 {
		tv[0].Sec = *(*int64)(unsafe.Pointer(times))
		tv[1].Sec = *(*int64)(unsafe.Pointer(times + 8))
	} else {
		tv[0].Sec = now.Unix()
		tv[0].Usec = int64(now.Nanosecond() / 1000)
		tv[1] = tv[0]
	}
	if err := syscall.Utimes(cstr(path), tv[:]); err != nil {
		return -1
	}
	return 0
}

//go:noinline
func goGethostname(name, length uintptr) int {
	h, err := os.Hostname()
	if err != nil {
		return -1
	}
	n := uintptr(len(h))
	if n > length-1 {
		n = length - 1
	}
	putStr(name, h[:n])
	st8(name+n, 0)
	return 0
}

// --- errno strings ----------------------------------------------------------

//go:noinline
func goStrerrorR(err, buf, buflen uintptr) int {
	msg := syscall.Errno(err).Error()
	if msg == "" {
		msg = "Unknown error"
	}
	n := uintptr(len(msg))
	if n > buflen-1 {
		n = buflen - 1
	}
	putStr(buf, msg[:n])
	st8(buf+n, 0)
	return 0
}

// __gnu_strerror_r returns a pointer to the message.
//
//go:noinline
func goGnuStrerrorR(err, buf, buflen uintptr) uintptr {
	msg := syscall.Errno(err).Error()
	if msg == "" {
		msg = "Unknown error"
	}
	if buf != 0 {
		n := uintptr(len(msg))
		if n > buflen-1 {
			n = buflen - 1
		}
		putStr(buf, msg[:n])
		st8(buf+n, 0)
		return buf
	}
	return cstringConst(msg)
}

// --- sockets (thin syscall pass-through) ------------------------------------

const (
	nSysSocket      = 41
	nSysConnect     = 42
	nSysAccept      = 43
	nSysSendto      = 44
	nSysRecvfrom    = 45
	nSysSendmsg     = 46
	nSysRecvmsg     = 47
	nSysShutdown    = 48
	nSysBind        = 49
	nSysListen      = 50
	nSysGetsockname = 51
	nSysGetpeername = 52
	nSysSocketpair  = 53
	nSysSetsockopt  = 54
	nSysGetsockopt  = 55
	nSysAccept4     = 288
	nSysRecvmmsg    = 299
	nSysSendmmsg    = 307
)

//go:noinline
func goSocket(domain, typ, proto uintptr) int {
	return int(int64(rawSys(nSysSocket, domain, typ, proto)))
}

//go:noinline
func goSocketpair(domain, typ, proto, sv uintptr) int {
	return int(int64(rawSys6(nSysSocketpair, domain, typ, proto, sv, 0, 0)))
}

//go:noinline
func goBind(fd, addr, addrlen uintptr) int {
	return int(int64(rawSys(nSysBind, fd, addr, addrlen)))
}

//go:noinline
func goListen(fd, backlog uintptr) int {
	return int(int64(rawSys(nSysListen, fd, backlog, 0)))
}

//go:noinline
func goAccept(fd, addr, addrlen uintptr) int {
	return int(int64(rawSys(nSysAccept, fd, addr, addrlen)))
}

//go:noinline
func goAccept4(fd, addr, addrlen, flags uintptr) int {
	return int(int64(rawSys6(nSysAccept4, fd, addr, addrlen, flags, 0, 0)))
}

//go:noinline
func goConnect(fd, addr, addrlen uintptr) int {
	return int(int64(rawSys(nSysConnect, fd, addr, addrlen)))
}

//go:noinline
func goSendto(fd, buf, n, flags, addr, addrlen uintptr) int {
	return int(int64(rawSys6(nSysSendto, fd, buf, n, flags, addr, addrlen)))
}

//go:noinline
func goRecvfrom(fd, buf, n, flags, addr, addrlen uintptr) int {
	return int(int64(rawSys6(nSysRecvfrom, fd, buf, n, flags, addr, addrlen)))
}

//go:noinline
func goSendmsg(fd, msg, flags uintptr) int {
	r := rawSys6(nSysSendmsg, fd, msg, flags, 0, 0, 0)
	return int(int64(r))
}

//go:noinline
func goRecvmsg(fd, msg, flags uintptr) int {
	r := rawSys6(nSysRecvmsg, fd, msg, flags, 0, 0, 0)
	return int(int64(r))
}

//go:noinline
func goSendmmsg(fd, msgvec, vlen, flags uintptr) int {
	return int(int64(rawSys6(nSysSendmmsg, fd, msgvec, vlen, flags, 0, 0)))
}

//go:noinline
func goRecvmmsg(fd, msgvec, vlen, flags, timeout uintptr) int {
	return int(int64(rawSys6(nSysRecvmmsg, fd, msgvec, vlen, flags, timeout, 0)))
}

//go:noinline
func goSetsockopt(fd, level, optname, optval, optlen uintptr) int {
	return int(int64(rawSys6(nSysSetsockopt, fd, level, optname, optval, optlen, 0)))
}

//go:noinline
func goGetsockopt(fd, level, optname, optval, optlen uintptr) int {
	return int(int64(rawSys6(nSysGetsockopt, fd, level, optname, optval, optlen, 0)))
}

//go:noinline
func goGetsockname(fd, addr, addrlen uintptr) int {
	r := rawSys6(nSysGetsockname, fd, addr, addrlen, 0, 0, 0)
	return int(int64(r))
}

//go:noinline
func goGetpeername(fd, addr, addrlen uintptr) int {
	r := rawSys6(nSysGetpeername, fd, addr, addrlen, 0, 0, 0)
	return int(int64(r))
}

//go:noinline
func goShutdown(fd, how uintptr) int {
	return int(int64(rawSys(nSysShutdown, fd, how, 0)))
}

// --- poll / epoll / timers --------------------------------------------------

//go:noinline
func goPoll(fds, nfds, timeout uintptr) int {
	return int(int64(rawSys(nSysPoll, fds, nfds, timeout)))
}

//go:noinline
func goSelect(nfds, r, w, e, timeout uintptr) int {
	return int(int64(rawSys6(nSysSelect, nfds, r, w, e, timeout, 0)))
}

//go:noinline
func goEpollCreate(size uintptr) int {
	return int(int64(rawSys(nSysEpollCreate, size, 0, 0)))
}

//go:noinline
func goEpollCreate1(flags uintptr) int {
	return int(int64(rawSys(nSysEpollCreate1, flags, 0, 0)))
}

//go:noinline
func goEpollCtl(epfd, op, fd, event uintptr) int {
	return int(int64(rawSys6(nSysEpollCtl, epfd, op, fd, event, 0, 0)))
}

//go:noinline
func goEpollWait(epfd, events, maxevents, timeout uintptr) int {
	return int(int64(rawSys6(nSysEpollWait, epfd, events, maxevents, timeout, 0, 0)))
}

//go:noinline
func goEventfd(initval, flags uintptr) int {
	return int(int64(rawSys(nSysEventfd2, initval, flags, 0)))
}

//go:noinline
func goTimerfdCreate(clockid, flags uintptr) int {
	return int(int64(rawSys(nSysTimerfdCreate, clockid, flags, 0)))
}

//go:noinline
func goTimerfdSettime(fd, flags, newval, oldval uintptr) int {
	return int(int64(rawSys6(nSysTimerfdSettime, fd, flags, newval, oldval, 0, 0)))
}

// --- signals ----------------------------------------------------------------
//
// The runtime does not yet deliver asynchronous signals into guest code.
// Installing a handler is recorded as a no-op and returning success: a
// synchronous signal the guest installs a handler for (SIGSEGV, SIGFPE) will
// still terminate the process, so this is not a silent lie for those. Asychronous
// delivery is future work and is documented as such.

//go:noinline
func goSignal(sig, handler uintptr) uintptr {
	return 0
}

//go:noinline
func goSigaction(sig, act, oldact uintptr) int {
	if oldact != 0 {
		for i := uintptr(0); i < 152; i += 8 {
			st64(oldact+i, 0)
		}
	}
	return 0
}

//go:noinline
func goSigaltstack(ss, old uintptr) int {
	if old != 0 {
		for i := uintptr(0); i < 24; i += 8 {
			st64(old+i, 0)
		}
	}
	return 0
}

// sigemptyset / sigfillset operate on bionic's 1024-bit sigset_t.
//
//go:noinline
func goSigemptyset(set uintptr) int {
	for i := uintptr(0); i < 128; i += 8 {
		st64(set+i, 0)
	}
	return 0
}

//go:noinline
func goSigfillset(set uintptr) int {
	for i := uintptr(0); i < 128; i += 8 {
		st64(set+i, ^uint64(0))
	}
	return 0
}

//go:noinline
func goSigismember(set, sig uintptr) int {
	w := (sig - 1) / 64
	b := (sig - 1) % 64
	if *(*uint64)(unsafe.Pointer(set + w*8))&(1<<b) != 0 {
		return 1
	}
	return 0
}

//go:noinline
func goRaise(sig uintptr) int { return 0 }

// --- pthread scheduling / rwlocks ------------------------------------------

//go:noinline
func goPthreadAttrGetstack(attr, stackaddr, stacksize uintptr) int {
	if stackaddr != 0 {
		*(*uintptr)(unsafe.Pointer(stackaddr)) = 0x1000
	}
	if stacksize != 0 {
		*(*uintptr)(unsafe.Pointer(stacksize)) = 8 << 20
	}
	return 0
}

//go:noinline
func goPthreadAttrSetschedparam(attr, param uintptr) int { return 0 }

//go:noinline
func goPthreadCondattrSetclock(attr, clock uintptr) int { return 0 }

//go:noinline
func goPthreadGetschedparam(thread, policy, param uintptr) int {
	if policy != 0 {
		st32(policy, 0)
	}
	if param != 0 {
		st32(param, 0)
	}
	return 0
}

//go:noinline
func goPthreadSetschedparam(thread, policy, param uintptr) int { return 0 }

//go:noinline
func goPthreadGetattrNp(thread, attr uintptr) int {
	for i := uintptr(0); i < 56; i += 8 {
		st64(attr+i, 0)
	}
	// Report a plausible stack. Callers mostly want the size.
	st64(attr+8, 0x1000)
	st64(attr+16, 8<<20)
	return 0
}

//go:noinline
func goPthreadExit(retval uintptr) {
	// With a single guest thread there is nothing to unwind to. Returning is
	// the least destructive behaviour; a real implementation would tear down
	// the thread's guest stack.
}

// A read/write lock degenerates to a plain lock when there is only one thread
// that ever holds it, which is the case today. The lock word lives in the
// guest's pthread_rwlock_t storage.
//
//go:noinline
func goRwlockInit(lock, attr uintptr) int {
	st32(lock, 0)
	return 0
}

//go:noinline
func goRwlockDestroy(lock uintptr) int { return 0 }

//go:noinline
func goRwlockRdlock(lock uintptr) int { return goPthreadMutexLock(lock) }

//go:noinline
func goRwlockWrlock(lock uintptr) int { return goPthreadMutexLock(lock) }

//go:noinline
func goRwlockUnlock(lock uintptr) int { return goPthreadMutexUnlock(lock) }

// --- directories ------------------------------------------------------------

// goOpenDir / goReadDir / goCloseDir implement a minimal, faithful DIR stream.
// The handle is a Go object kept alive by dirMu; the guest only ever passes its
// address back, so its contents are opaque to it.

type dirHandle struct {
	entries []os.FileInfo
	pos     int
	// dirent is the reusable buffer whose address readdir returns.
	dirent [280]byte
}

var (
	dirMu        sync.Mutex
	dirHandleMap = map[uintptr]*dirHandle{}
)

//go:noinline
func goOpenDir(path uintptr) uintptr {
	f, err := os.Open(cstr(path))
	if err != nil {
		return 0
	}
	names, err := f.Readdir(-1)
	f.Close()
	if err != nil {
		return 0
	}
	h := &dirHandle{entries: names}
	p := uintptr(unsafe.Pointer(h))
	dirMu.Lock()
	dirHandleMap[p] = h
	dirMu.Unlock()
	return p
}

//go:noinline
func goReadDir(dirp uintptr) uintptr {
	dirMu.Lock()
	h := dirHandleMap[dirp]
	dirMu.Unlock()
	if h == nil {
		return 0
	}
	if h.pos >= len(h.entries) {
		return 0
	}
	e := h.entries[h.pos]
	h.pos++
	// struct dirent (bionic, 64-bit): d_ino u64, d_off i64, d_reclen u16,
	// d_type u8, d_name[256].
	st64(uintptr(unsafe.Pointer(&h.dirent[0]))+0, 0)
	st64(uintptr(unsafe.Pointer(&h.dirent[0]))+8, uint64(h.pos))
	st16(uintptr(unsafe.Pointer(&h.dirent[0]))+16, 280)
	dt := byte(0) // DT_UNKNOWN
	if e.IsDir() {
		dt = 4 // DT_DIR
	} else if e.Mode().IsRegular() {
		dt = 8 // DT_REG
	}
	st8(uintptr(unsafe.Pointer(&h.dirent[0]))+18, dt)
	name := e.Name()
	n := uintptr(len(name))
	if n > 255 {
		n = 255
	}
	putStr(uintptr(unsafe.Pointer(&h.dirent[0]))+19, name[:n])
	st8(uintptr(unsafe.Pointer(&h.dirent[0]))+19+n, 0)
	return uintptr(unsafe.Pointer(&h.dirent[0]))
}

//go:noinline
func goCloseDir(dirp uintptr) int {
	dirMu.Lock()
	delete(dirHandleMap, dirp)
	dirMu.Unlock()
	return 0
}

// init registers the symbols implemented in this file.
func init() {
	reg := func(name string, fn any) { hostFuncs[name] = fn }

	reg("sysinfo", goSysinfo)
	reg("uname", goUname)
	reg("system", goSystem)
	reg("__assert", goAssert)
	reg("__system_property_get", goSystemPropertyGet)

	reg("waitpid", goWaitpid)
	reg("fork", goFork)
	reg("execve", goExecve)
	reg("execv", goExecv)

	reg("sched_getcpu", goSchedGetcpu)
	reg("sched_get_priority_max", goSchedGetPriorityMax)
	reg("sched_get_priority_min", goSchedGetPriorityMin)
	reg("sched_getscheduler", goSchedGetscheduler)
	reg("sched_setscheduler", goSchedSetscheduler)
	reg("sched_getparam", goSchedGetparam)
	reg("getpriority", goGetpriority)
	reg("setpriority", goSetpriority)
	reg("prctl", goPrctl)
	reg("ptrace", goPtrace)

	reg("mlock", goMlock)
	reg("munlock", goMunlock)
	reg("msync", goMsync)
	reg("fsync", goFsync)
	reg("fdatasync", goFdatasync)
	reg("sync", goSync)
	reg("posix_fallocate", goPosixFallocate)
	reg("mallinfo", goMallinfo)

	reg("pipe", goPipe)
	reg("pipe2", goPipe2)
	reg("writev", goWritev)
	reg("readv", goReadv)
	reg("ioctl", goIoctl)

	reg("arc4random_buf", goArc4randomBuf)
	reg("realpath", goRealpath)
	reg("__readlink_chk", goReadlinkChk)
	reg("statvfs", goStatvfs)
	reg("utimes", goUtimes)
	reg("utime", goUtime)
	reg("gethostname", goGethostname)
	reg("strerror_r", goStrerrorR)
	reg("__gnu_strerror_r", goGnuStrerrorR)

	reg("socket", goSocket)
	reg("socketpair", goSocketpair)
	reg("bind", goBind)
	reg("listen", goListen)
	reg("accept", goAccept)
	reg("accept4", goAccept4)
	reg("connect", goConnect)
	reg("sendto", goSendto)
	reg("recvfrom", goRecvfrom)
	reg("sendmsg", goSendmsg)
	reg("recvmsg", goRecvmsg)
	reg("sendmmsg", goSendmmsg)
	reg("recvmmsg", goRecvmmsg)
	reg("setsockopt", goSetsockopt)
	reg("getsockopt", goGetsockopt)
	reg("getsockname", goGetsockname)
	reg("getpeername", goGetpeername)
	reg("shutdown", goShutdown)

	reg("poll", goPoll)
	reg("select", goSelect)
	reg("epoll_create", goEpollCreate)
	reg("epoll_create1", goEpollCreate1)
	reg("epoll_ctl", goEpollCtl)
	reg("epoll_wait", goEpollWait)
	reg("eventfd", goEventfd)
	reg("timerfd_create", goTimerfdCreate)
	reg("timerfd_settime", goTimerfdSettime)

	reg("signal", goSignal)
	reg("sigaction", goSigaction)
	reg("sigaltstack", goSigaltstack)
	reg("sigemptyset", goSigemptyset)
	reg("sigfillset", goSigfillset)
	reg("sigismember", goSigismember)
	reg("raise", goRaise)

	reg("pthread_attr_getstack", goPthreadAttrGetstack)
	reg("pthread_attr_setschedparam", goPthreadAttrSetschedparam)
	reg("pthread_condattr_setclock", goPthreadCondattrSetclock)
	reg("pthread_getschedparam", goPthreadGetschedparam)
	reg("pthread_setschedparam", goPthreadSetschedparam)
	reg("pthread_getattr_np", goPthreadGetattrNp)
	reg("pthread_exit", goPthreadExit)
	reg("pthread_rwlock_init", goRwlockInit)
	reg("pthread_rwlock_destroy", goRwlockDestroy)
	reg("pthread_rwlock_rdlock", goRwlockRdlock)
	reg("pthread_rwlock_wrlock", goRwlockWrlock)
	reg("pthread_rwlock_unlock", goRwlockUnlock)

	reg("opendir", goOpenDir)
	reg("readdir", goReadDir)
	reg("closedir", goCloseDir)
}
