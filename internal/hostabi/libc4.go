package hostabi

import (
	"fmt"
	"math"
	"net"
	"sync"
	"unsafe"
)

// Networking and libc entry points the engine imports that were still bound
// to abort traps: the legacy resolver (gethostbyname), interface index
// lookups, the fortified send/pread variants, control-message iteration,
// termios, syslog, vasprintf and a few math helpers. They are what WebRTC
// (voice chat) and the engine's socket code use, beyond the core already in
// net.go and libc3.go.

func init() {
	reg := func(name string, fn any) { hostFuncs[name] = fn }
	reg("gethostbyname", goGethostbyname)
	reg("if_nametoindex", goIfNametoindex)
	reg("if_indextoname", goIfIndextoname)
	reg("__sendto_chk", goSendtoChk)
	reg("__pread_chk", goPreadChk)
	reg("__cmsg_nxthdr", goCmsgNxthdr)
	reg("tcgetattr", goTcgetattr)
	reg("tcsetattr", goTcsetattr)
	reg("openlog", goOpenlog)
	reg("closelog", goCloselog)
	reg("vasprintf", goVasprintf)
	reg("scalbn", goScalbn)
	reg("getopt_long", goGetoptLong)

	// ldiv returns ldiv_t {quot, rem} in RAX:RDX, which a Go thunk cannot
	// produce, and that is exactly what IDIV leaves behind. Built at package
	// init, before any guest code runs (see codeArena).
	code := []byte{
		0x48, 0x89, 0xF8, // MOVQ DI, AX
		0x48, 0x99, //       CQO (sign-extend RAX into RDX)
		0x48, 0xF7, 0xFE, // IDIVQ SI: RAX = quotient, RDX = remainder
		0xC3, //             RET
	}
	b := codeArena(len(code))
	copy(b, code)
	arenaMakeExec()
	rawSyms["ldiv"] = uintptr(unsafe.Pointer(&b[0]))
}

// --- resolver -------------------------------------------------------------

// struct hostent (bionic and glibc agree on x86_64).
type hostent struct {
	name     uintptr // char*
	aliases  uintptr // char**
	addrtype int32
	length   int32
	addrList uintptr // char**
}

// gethostbyname returns a pointer to static storage, as in C; the previous
// result is replaced by the next call.
var gethost struct {
	mu      sync.Mutex
	ent     hostent
	name    []byte
	aliases [1]uintptr
	addrs   [][4]byte
	list    []uintptr
}

//go:noinline
func goGethostbyname(name uintptr) uintptr {
	if name == 0 {
		return 0
	}
	n := cstr(name)
	rc, ips, _, canon := resolveOffStack(n, "", 0, afInet, 0)
	var v4 [][4]byte
	for _, ip := range ips {
		if p := net.IP(ip).To4(); p != nil {
			v4 = append(v4, [4]byte{p[0], p[1], p[2], p[3]})
		}
	}
	if debugNet {
		writeStderrRaw(fmt.Sprintf("rbxrt: net: gethostbyname(%q) -> rc=%d %d addrs\n", n, rc, len(v4)))
	}
	if rc != 0 || len(v4) == 0 {
		return 0
	}
	if canon == "" {
		canon = n
	}
	g := &gethost
	g.mu.Lock()
	defer g.mu.Unlock()
	g.name = append([]byte(canon), 0)
	g.addrs = v4
	g.list = make([]uintptr, len(v4)+1)
	for i := range g.addrs {
		g.list[i] = uintptr(unsafe.Pointer(&g.addrs[i][0]))
	}
	g.aliases[0] = 0
	g.ent = hostent{
		name:     uintptr(unsafe.Pointer(&g.name[0])),
		aliases:  uintptr(unsafe.Pointer(&g.aliases[0])),
		addrtype: afInet,
		length:   4,
		addrList: uintptr(unsafe.Pointer(&g.list[0])),
	}
	return uintptr(unsafe.Pointer(&g.ent))
}

// --- network interfaces ---------------------------------------------------

//go:noinline
func goIfNametoindex(name uintptr) uintptr {
	if name == 0 {
		return 0
	}
	ifc, err := net.InterfaceByName(cstr(name))
	if err != nil {
		setErrno(19) // ENODEV
		return 0
	}
	return uintptr(ifc.Index)
}

//go:noinline
func goIfIndextoname(index, buf uintptr) uintptr {
	ifc, err := net.InterfaceByIndex(int(uint32(index)))
	if err != nil || buf == 0 || len(ifc.Name) >= 16 { // IF_NAMESIZE
		setErrno(6) // ENXIO
		return 0
	}
	copyToGuest(buf, append([]byte(ifc.Name), 0))
	return buf
}

// --- fortified variants ---------------------------------------------------

// __sendto_chk(fd, buf, len, buflen, flags, addr, addrlen): sendto with a
// check that len fits the source buffer.
//
//go:noinline
func goSendtoChk(fd, buf, n, buflen, flags, addr, addrlen uintptr) int {
	if n > buflen {
		fortifyFail("__sendto_chk")
	}
	return goSendto(fd, buf, n, flags, addr, addrlen)
}

// __pread_chk(fd, buf, count, offset, buflen).
//
//go:noinline
func goPreadChk(fd, buf, n, off, buflen uintptr) int {
	if n > buflen {
		fortifyFail("__pread_chk")
	}
	return goPread(fd, buf, n, off)
}

// --- control messages -----------------------------------------------------

// __cmsg_nxthdr(msghdr*, cmsghdr*) is CMSG_NXTHDR: the control message after
// cmsg, or NULL when it would run past msg_control + msg_controllen.
// struct msghdr: msg_control at 32, msg_controllen (size_t) at 40;
// struct cmsghdr: cmsg_len (size_t), level, type: 16 bytes.
//
//go:noinline
func goCmsgNxthdr(msg, cmsg uintptr) uintptr {
	if msg == 0 || cmsg == 0 {
		return 0
	}
	const hdr = 16
	align := func(n uintptr) uintptr { return (n + 7) &^ 7 }
	clen := ld64(cmsg)
	if clen < hdr {
		return 0
	}
	next := cmsg + align(clen)
	end := ld64(msg+32) + ld64(msg+40)
	if next+hdr > end || next+align(ld64(next)) > end {
		return 0
	}
	return next
}

// --- termios --------------------------------------------------------------

// bionic's struct termios is the kernel's, so TCGETS/TCSETS* pass it through.
//
//go:noinline
func goTcgetattr(fd, t uintptr) int {
	return goIoctl(fd, 0x5401, t) // TCGETS
}

//go:noinline
func goTcsetattr(fd, action, t uintptr) int {
	if action > 2 {
		setErrno(22) // EINVAL
		return -1
	}
	return goIoctl(fd, 0x5402+action, t) // TCSETS, TCSETSW, TCSETSF
}

// --- syslog ---------------------------------------------------------------

// The engine has no syslog daemon to talk to on Android either (it logs to
// logcat); openlog/closelog are accepted and syslog messages go to stderr.
//
//go:noinline
func goOpenlog(ident, option, facility uintptr) {}

//go:noinline
func goCloselog() {}

// --- vasprintf ------------------------------------------------------------

var guestMallocFn uintptr

// guestMalloc allocates from the engine's own allocator, so the guest may
// free() the result.
func guestMalloc(n uintptr) uintptr {
	if guestMallocFn == 0 && dlHooks.Sym != nil {
		guestMallocFn = dlHooks.Sym(0, "malloc")
	}
	if guestMallocFn == 0 {
		return 0
	}
	return invokeGuest(guestMallocFn, n, 0, 0)
}

// vasprintf(char **strp, fmt, va_list): format into a malloc'd string.
//
//go:noinline
func goVasprintf(strp, format, ap uintptr) int {
	if strp == 0 || format == 0 {
		return -1
	}
	out := formatC(cstr(format), (*vaList)(unsafe.Pointer(ap)))
	p := guestMalloc(uintptr(len(out) + 1))
	if p == 0 {
		*(*uintptr)(unsafe.Pointer(strp)) = 0
		setErrno(12) // ENOMEM
		return -1
	}
	copyToGuest(p, out)
	*(*byte)(unsafe.Pointer(p + uintptr(len(out)))) = 0
	*(*uintptr)(unsafe.Pointer(strp)) = p
	return len(out)
}

// --- math -----------------------------------------------------------------

// scalbn(double x, int n): x in XMM0 and n in DI reach Go as (x, n), since
// the two ABIs number float and integer registers independently.
//
//go:noinline
func goScalbn(x float64, n int32) float64 {
	return math.Ldexp(x, int(n))
}

// --- getopt_long ----------------------------------------------------------

// The engine links a command-line parser it never runs on a device (there
// is no argv for an Android app). Report "no more options".
//
//go:noinline
func goGetoptLong(argc, argv, optstring, longopts, longindex uintptr) int {
	return -1
}
