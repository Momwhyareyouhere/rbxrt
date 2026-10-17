package hostabi

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// This file implements the Android NDK Looper (android/looper.h) that the
// engine's event threads build on. Unlike the rest of the host ABI, ALooper has
// no Linux equivalent to forward to: it is a thin event loop over epoll, with a
// per-thread wake pipe so another thread can break a blocked poll. It is
// implemented properly rather than stubbed, because the engine drives its main
// loop (and, later, its task scheduler) through it.
//
// A looper belongs to the "thread" that created or prepared it. Guest threads
// are goroutines here (see internal/hostabi/stack.go), and the runtime already
// keys per-thread state on the goroutine pointer (getg()), so the looper table
// does the same: one looper per guest thread.

// ALooper event and poll-result constants (android/looper.h).
const (
	aLooperPrepareAllowNonCallbacks = 1 << 0

	aLooperEventInput   = 1 << 0
	aLooperEventOutput  = 1 << 1
	aLooperEventError   = 1 << 2
	aLooperEventHangup  = 1 << 3
	aLooperEventInvalid = 1 << 4

	aLooperPollWake     = -1
	aLooperPollCallback = -2
	aLooperPollTimeout  = -3
	aLooperPollError    = -4
)

var debugLooper = os.Getenv("RBXRT_DEBUG_LOOPER") != ""

// looperPollCount rate-limits the ALooper_pollOnce trace so a busy main loop
// does not flood the log.
var looperPollCount uint64

// looperFd is one fd registered with a looper. When callback is non-zero the
// looper invokes it on readiness (guest code, called back through CallGuestInt3);
// otherwise the fd is reported to the caller of ALooper_pollOnce via its out
// parameters, identified by ident.
type looperFd struct {
	ident    int
	events   int
	callback uintptr
	data     uintptr
}

// aLooper is one event loop. The wake pipe is registered in the epoll set so
// ALooper_wake can interrupt a poll from another thread.
type aLooper struct {
	epfd  int
	wakeR int
	wakeW int

	mu   sync.Mutex
	fds  map[int]*looperFd
	refs int
}

var (
	looperMu  sync.Mutex
	looperByG = map[uintptr]*aLooper{} // keyed by goroutine (guest thread) pointer
	looperAll = map[uintptr]*aLooper{} // keyed by the handle handed to the guest
)

// newALooper creates a looper with an epoll set and a non-blocking wake pipe.
func newALooper() *aLooper {
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper: epoll_create1: %v\n", err)
		return nil
	}
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper: pipe2: %v\n", err)
		syscall.Close(epfd)
		return nil
	}
	ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(p[0])}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, p[0], &ev); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper: epoll_ctl(wake): %v\n", err)
		syscall.Close(p[0])
		syscall.Close(p[1])
		syscall.Close(epfd)
		return nil
	}
	lp := &aLooper{epfd: epfd, wakeR: p[0], wakeW: p[1], fds: map[int]*looperFd{}, refs: 1}
	if debugLooper {
		fmt.Fprintf(os.Stderr, "rbxrt: newALooper looper=%#x epfd=%d wakeR=%d wakeW=%d\n",
			looperHandle(lp), lp.epfd, lp.wakeR, lp.wakeW)
	}
	return lp
}

// looperHandle returns the opaque value handed to guest code for lp.
func looperHandle(lp *aLooper) uintptr { return uintptr(unsafe.Pointer(lp)) }

// looperAt resolves a guest-supplied looper handle.
func looperAt(h uintptr) *aLooper {
	looperMu.Lock()
	defer looperMu.Unlock()
	return looperAll[h]
}

// looperForThread returns the calling thread's looper, which is what
// ALooper_pollOnce and ALooper_pollAll operate on: neither takes a looper
// argument, unlike addFd/removeFd/wake.
func looperForThread() *aLooper {
	g := getg()
	looperMu.Lock()
	defer looperMu.Unlock()
	return looperByG[g]
}

// --- NDK entry points ----------------------------------------------------

//go:noinline
func goALooperForThread() uintptr {
	// The NDK contract is "return this thread's looper, or NULL if it has none",
	// but on Android the framework (ActivityThread) prepares the main thread's
	// looper before any activity code runs, and the engine relies on that. Our
	// runtime is standing in for the framework, so forThread prepares the
	// calling thread's looper if it does not exist yet; it must never return
	// NULL to a thread that has not called prepare explicitly.
	g := getg()
	looperMu.Lock()
	lp := looperByG[g]
	if lp == nil {
		lp = newALooper()
		if lp != nil {
			looperByG[g] = lp
			looperAll[looperHandle(lp)] = lp
			if debugLooper {
				fmt.Fprintf(os.Stderr, "rbxrt: ALooper_forThread: prepared looper %#x for thread %#x (framework stand-in)\n", looperHandle(lp), g)
			}
		}
	}
	looperMu.Unlock()
	if lp == nil {
		return 0
	}
	return looperHandle(lp)
}

//go:noinline
func goALooperPrepare(opts uintptr) uintptr {
	if opts&^uintptr(aLooperPrepareAllowNonCallbacks) != 0 && debugLooper {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper_prepare: unknown opts %#x\n", opts)
	}
	g := getg()
	looperMu.Lock()
	lp := looperByG[g]
	if lp == nil {
		lp = newALooper()
		if lp == nil {
			looperMu.Unlock()
			return 0
		}
		looperByG[g] = lp
		looperAll[looperHandle(lp)] = lp
	}
	looperMu.Unlock()
	if debugLooper {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper_prepare(opts=%#x) -> %#x g=%#x\n", opts, looperHandle(lp), g)
	}
	return looperHandle(lp)
}

//go:noinline
func goALooperAcquire(h uintptr) {
	lp := looperAt(h)
	if lp == nil {
		return
	}
	lp.mu.Lock()
	lp.refs++
	lp.mu.Unlock()
}

//go:noinline
func goALooperRelease(h uintptr) {
	lp := looperAt(h)
	if lp == nil {
		return
	}
	// The thread that prepared the looper owns a reference (refs starts at 1),
	// so releasing an acquired reference leaves the looper usable. We do not
	// tear a looper down while its thread lives.
	lp.mu.Lock()
	lp.refs--
	lp.mu.Unlock()
}

//go:noinline
func goALooperAddFd(h, fd, ident, events, callback, data uintptr) int {
	lp := looperAt(h)
	if lp == nil {
		return -1
	}
	f := int(fd)
	lp.mu.Lock()
	lp.fds[f] = &looperFd{ident: int(int64(ident)), events: int(events), callback: callback, data: data}
	lp.mu.Unlock()

	ev := syscall.EpollEvent{Events: epollEventsFromALooper(uint32(events)), Fd: int32(f)}
	err := syscall.EpollCtl(lp.epfd, syscall.EPOLL_CTL_ADD, f, &ev)
	if err == syscall.EEXIST {
		err = syscall.EpollCtl(lp.epfd, syscall.EPOLL_CTL_MOD, f, &ev)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper_addFd(fd=%d ident=%d events=%#x): epoll_ctl: %v\n", f, int64(ident), events, err)
		return -1
	}
	if debugLooper {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper_addFd(looper=%#x fd=%d ident=%d events=%#x cb=%#x data=%#x g=%#x)\n",
			h, f, int64(ident), events, callback, data, getg())
	}
	return 1
}

//go:noinline
func goALooperRemoveFd(h, fd uintptr) int {
	lp := looperAt(h)
	if lp == nil {
		return -1
	}
	f := int(fd)
	lp.mu.Lock()
	_, ok := lp.fds[f]
	delete(lp.fds, f)
	lp.mu.Unlock()
	syscall.EpollCtl(lp.epfd, syscall.EPOLL_CTL_DEL, f, nil)
	if debugLooper {
		fmt.Fprintf(os.Stderr, "rbxrt: ALooper_removeFd(looper=%#x fd=%d) found=%t\n", h, f, ok)
	}
	return 0
}

//go:noinline
func goALooperWake(h uintptr) {
	lp := looperAt(h)
	if lp == nil {
		return
	}
	syscall.Write(lp.wakeW, []byte{0})
}

//go:noinline
func goALooperPollOnce(timeoutMillis, outFd, outEvents, outData uintptr) int {
	lp := looperForThread()
	if lp == nil {
		return aLooperPollError
	}
	h := looperHandle(lp)
	timeout := int(int64(timeoutMillis))
	if debugLooper {
		looperPollCount++
		if looperPollCount <= 10 || looperPollCount%50000 == 1 {
			fmt.Fprintf(os.Stderr, "rbxrt: ALooper_pollOnce h=%#x timeout=%d n=%d g=%#x\n", h, timeout, looperPollCount, getg())
		}
	}
	events := make([]syscall.EpollEvent, 16)
	for {
		n, err := syscall.EpollWait(lp.epfd, events, timeout)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			fmt.Fprintf(os.Stderr, "rbxrt: ALooper_pollOnce: epoll_wait: %v\n", err)
			clearPollOut(outFd, outEvents, outData)
			return aLooperPollError
		}
		if n == 0 {
			clearPollOut(outFd, outEvents, outData)
			return aLooperPollTimeout
		}

		woke := false
		type pending struct {
			fd     int
			events int
			req    *looperFd
		}
		var callbacks []pending
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == lp.wakeR {
				drainPipe(lp.wakeR)
				woke = true
				continue
			}
			lp.mu.Lock()
			req := lp.fds[fd]
			lp.mu.Unlock()
			if req == nil {
				continue
			}
			evs := aLooperEventsFromEpoll(events[i].Events)
			if req.callback != 0 {
				callbacks = append(callbacks, pending{fd: fd, events: evs, req: req})
				continue
			}
			// No callback: report the fd (and its data) to the caller and
			// return its ident, exactly as Looper::pollOnce does. Each output
			// is guarded individually because callers legitimately pass NULL
			// for the ones they do not want: native_app_glue polls with
			// outFd == NULL and reads `ident` plus *outData instead. The
			// report must happen even so, or the fd stays readable, epoll_wait
			// returns it again immediately, and this loop never makes progress.
			if outFd != 0 {
				st32(outFd, uint32(int32(req.ident)))
			}
			if outEvents != 0 {
				st32(outEvents, uint32(int32(evs)))
			}
			if outData != 0 {
				stPtr(outData, req.data)
			}
			return req.ident
		}

		// Invoke callbacks without holding lp.mu: a callback is guest code and
		// may call back into the looper.
		for _, c := range callbacks {
			if debugLooper {
				fmt.Fprintf(os.Stderr, "rbxrt: ALooper callback fd=%d ident=%d events=%#x cb=%#x\n",
					c.fd, c.req.ident, c.events, c.req.callback)
			}
			if CallGuestInt3(c.req.callback, uintptr(c.fd), uintptr(c.events), c.req.data) == 0 {
				goALooperRemoveFd(h, uintptr(c.fd))
			}
		}

		if woke {
			clearPollOut(outFd, outEvents, outData)
			return aLooperPollWake
		}
		if len(callbacks) > 0 {
			clearPollOut(outFd, outEvents, outData)
			return aLooperPollCallback
		}
		// Only unknown fds were reported; keep waiting if we are allowed to.
		if timeout == 0 {
			clearPollOut(outFd, outEvents, outData)
			return aLooperPollTimeout
		}
	}
}

// clearPollOut mirrors Looper::pollOnce, which zeroes the out parameters before
// returning any result other than a file descriptor's ident. native_app_glue
// relies on this: it treats a non-negative return as "a source is ready" and
// dereferences *outData, so a stale pointer there would be a wild read.
func clearPollOut(outFd, outEvents, outData uintptr) {
	if outFd != 0 {
		st32(outFd, 0)
	}
	if outEvents != 0 {
		st32(outEvents, 0)
	}
	if outData != 0 {
		stPtr(outData, 0)
	}
}

//go:noinline
func goALooperPollAll(timeoutMillis, outFd, outEvents, outData uintptr) int {
	for {
		r := goALooperPollOnce(timeoutMillis, outFd, outEvents, outData)
		if r != aLooperPollCallback {
			return r
		}
	}
}

// --- helpers -------------------------------------------------------------

// drainPipe empties a non-blocking pipe's read end.
func drainPipe(fd int) {
	var buf [64]byte
	for {
		if _, err := syscall.Read(fd, buf[:]); err != nil {
			return
		}
	}
}

func epollEventsFromALooper(ev uint32) uint32 {
	var e uint32
	if ev&aLooperEventInput != 0 {
		e |= syscall.EPOLLIN
	}
	if ev&aLooperEventOutput != 0 {
		e |= syscall.EPOLLOUT
	}
	return e
}

func aLooperEventsFromEpoll(ev uint32) int {
	e := 0
	if ev&syscall.EPOLLIN != 0 {
		e |= aLooperEventInput
	}
	if ev&syscall.EPOLLOUT != 0 {
		e |= aLooperEventOutput
	}
	if ev&syscall.EPOLLERR != 0 {
		e |= aLooperEventError
	}
	if ev&syscall.EPOLLHUP != 0 {
		e |= aLooperEventHangup
	}
	return e
}

func init() {
	hostFuncs["ALooper_forThread"] = goALooperForThread
	hostFuncs["ALooper_prepare"] = goALooperPrepare
	hostFuncs["ALooper_acquire"] = goALooperAcquire
	hostFuncs["ALooper_release"] = goALooperRelease
	hostFuncs["ALooper_addFd"] = goALooperAddFd
	hostFuncs["ALooper_removeFd"] = goALooperRemoveFd
	hostFuncs["ALooper_pollOnce"] = goALooperPollOnce
	hostFuncs["ALooper_pollAll"] = goALooperPollAll
	hostFuncs["ALooper_wake"] = goALooperWake
}
