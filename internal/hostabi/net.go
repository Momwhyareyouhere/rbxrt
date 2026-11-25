package hostabi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

//go:noinline
func goInetPton(af, src, dst uintptr) int {
	if src == 0 || dst == 0 {
		return 0
	}
	ip := net.ParseIP(cstr(src))
	if ip == nil {
		return 0
	}
	switch af {
	case 2: // AF_INET
		v4 := ip.To4()
		if v4 == nil {
			return 0
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(dst)), 4), v4)
	case 10: // AF_INET6
		v6 := ip.To16()
		if v6 == nil || ip.To4() != nil {
			return 0
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(dst)), 16), v6)
	default:
		setErrno(97) // EAFNOSUPPORT
		return -1
	}
	return 1
}

// Bionic's <netdb.h> values. They are the BSD ones: EAI_* error codes are
// positive (glibc's are negative), and the AI_*/NI_* flag bits differ from
// glibc's too, so none of them may be taken from the host headers.
const (
	eaiAgain   = 2
	eaiFail    = 4
	eaiFamily  = 5
	eaiMemory  = 6
	eaiNoname  = 8
	eaiService = 9
	eaiSocktyp = 10
	eaiOverflw = 14

	aiPassive     = 0x1
	aiCanonname   = 0x2
	aiNumerichost = 0x4
	aiNumericserv = 0x8

	niNumerichost = 0x2
	niNamereqd    = 0x4
	niNumericserv = 0x8
	niDgram       = 0x10

	afUnspec = 0
	afInet   = 2
	afInet6  = 10

	sockStream = 1
	sockDgram  = 2
	ipprotoTCP = 6
	ipprotoUDP = 17
)

var debugNet = os.Getenv("RBXRT_DEBUG_NET") != ""

// goResolver is Go's own DNS resolver (it reads /etc/resolv.conf and
// /etc/hosts itself), so a lookup never goes through the host C library.
var goResolver = &net.Resolver{PreferGo: true}

// goGetnameinfo implements bionic's getnameinfo for numeric results and AF_UNIX.
// Reverse DNS is not performed: with NI_NAMEREQD that is reported as
// EAI_NONAME rather than inventing a name, and otherwise the numeric form is
// returned, which is what getnameinfo does when no name is found.
//
// C getnameinfo has seven integer/pointer arguments. mkThunk forwards the
// seventh stack argument into Go's seventh integer argument register.
//
//go:noinline
func goGetnameinfo(sa, salen, host, hostlen, serv, servlen, flags uintptr) int {
	if sa == 0 || salen < 2 {
		return eaiFamily
	}
	family := *(*uint16)(unsafe.Pointer(sa))
	var hostname, service string
	switch family {
	case afInet:
		if salen < 16 {
			return eaiFamily
		}
		p := unsafe.Slice((*byte)(unsafe.Pointer(sa)), 16)
		hostname = net.IPv4(p[4], p[5], p[6], p[7]).String()
		service = strconv.Itoa(int(binary.BigEndian.Uint16(p[2:4])))
	case afInet6:
		if salen < 28 {
			return eaiFamily
		}
		p := unsafe.Slice((*byte)(unsafe.Pointer(sa)), 28)
		hostname = net.IP(append([]byte(nil), p[8:24]...)).String()
		service = strconv.Itoa(int(binary.BigEndian.Uint16(p[2:4])))
	case 1: // AF_UNIX
		p := unsafe.Slice((*byte)(unsafe.Pointer(sa+2)), int(salen-2))
		for i, b := range p {
			if b == 0 {
				p = p[:i]
				break
			}
		}
		hostname = string(p)
	default:
		return eaiFamily
	}
	if flags&niNamereqd != 0 && family != 1 {
		return eaiNoname // no reverse lookup is performed
	}
	if host != 0 && hostlen != 0 {
		if uintptr(len(hostname))+1 > hostlen {
			return eaiOverflw
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(host)), len(hostname)), hostname)
		*(*byte)(unsafe.Pointer(host + uintptr(len(hostname)))) = 0
	}
	if serv != 0 && servlen != 0 {
		if uintptr(len(service))+1 > servlen {
			return eaiOverflw
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(serv)), len(service)), service)
		*(*byte)(unsafe.Pointer(serv + uintptr(len(service)))) = 0
	}
	return 0
}

// addrinfo is bionic's struct addrinfo (48 bytes on LP64).
type addrinfo struct {
	ai_flags     int32
	ai_family    int32
	ai_socktype  int32
	ai_protocol  int32
	ai_addrlen   uint32
	_pad         uint32
	ai_canonname *byte
	ai_addr      *byte
	ai_next      *addrinfo
}

// addrInfoBlocks keeps every result list reachable from Go while the guest
// holds it (the GC must not reclaim memory the guest points into); freeaddrinfo
// releases it.
var (
	addrInfoMu     sync.Mutex
	addrInfoBlocks = map[uintptr][]byte{}
)

// goGetaddrinfo is bionic's getaddrinfo, backed by Go's resolver. It honours
// hints (family, socktype, protocol, AI_PASSIVE, AI_NUMERICHOST,
// AI_NUMERICSERV, AI_CANONNAME) and returns one entry per address and socket
// type, IPv4 first.
//
//go:noinline
func goGetaddrinfo(node, service, hints, res uintptr) int {
	if res == 0 || (node == 0 && service == 0) {
		return eaiNoname
	}
	*(*uintptr)(unsafe.Pointer(res)) = 0
	var hFlags, hFamily, hSocktype, hProto int32
	if hints != 0 {
		h := (*addrinfo)(unsafe.Pointer(hints))
		hFlags, hFamily, hSocktype, hProto = h.ai_flags, h.ai_family, h.ai_socktype, h.ai_protocol
	}
	if hFamily != afUnspec && hFamily != afInet && hFamily != afInet6 {
		return eaiFamily
	}
	if hSocktype != 0 && hSocktype != sockStream && hSocktype != sockDgram {
		return eaiSocktyp
	}
	nodeStr, servStr := "", ""
	if node != 0 {
		nodeStr = cstr(node)
	}
	if service != 0 {
		servStr = cstr(service)
	}
	rc, ips, port, canon := resolveOffStack(nodeStr, servStr, hFlags, hFamily, hSocktype)
	if debugNet {
		writeStderrRaw(fmt.Sprintf("rbxrt: net: getaddrinfo(%q, %q, family=%d socktype=%d flags=%#x) -> rc=%d %v port=%d\n",
			nodeStr, servStr, hFamily, hSocktype, hFlags, rc, ips, port))
	}
	if rc != 0 {
		return rc
	}

	type ent struct {
		ip       net.IP
		socktype int32
		proto    int32
	}
	var ents []ent
	for _, ip := range ips {
		for _, st := range []int32{sockStream, sockDgram} {
			if hSocktype != 0 && hSocktype != st {
				continue
			}
			proto := int32(ipprotoTCP)
			if st == sockDgram {
				proto = ipprotoUDP
			}
			if hProto != 0 && hProto != proto {
				continue
			}
			ents = append(ents, ent{ip, st, proto})
		}
	}
	if len(ents) == 0 {
		return eaiNoname
	}
	const aiSize, saSize = 48, 28
	blk := make([]byte, len(ents)*(aiSize+saSize)+len(canon)+1)
	base := uintptr(unsafe.Pointer(&blk[0]))
	var canonPtr *byte
	if hFlags&aiCanonname != 0 && canon != "" {
		off := len(ents) * (aiSize + saSize)
		copy(blk[off:], canon)
		canonPtr = &blk[off]
	}
	var prev *addrinfo
	for i, e := range ents {
		ai := (*addrinfo)(unsafe.Pointer(base + uintptr(i*(aiSize+saSize))))
		sa := blk[i*(aiSize+saSize)+aiSize : (i+1)*(aiSize+saSize)]
		ai.ai_socktype = e.socktype
		ai.ai_protocol = e.proto
		ai.ai_flags = hFlags
		binary.BigEndian.PutUint16(sa[2:4], port)
		if v4 := e.ip.To4(); v4 != nil {
			ai.ai_family = afInet
			ai.ai_addrlen = 16
			binary.LittleEndian.PutUint16(sa[0:2], afInet)
			copy(sa[4:8], v4)
		} else {
			ai.ai_family = afInet6
			ai.ai_addrlen = 28
			binary.LittleEndian.PutUint16(sa[0:2], afInet6)
			copy(sa[8:24], e.ip.To16())
		}
		ai.ai_addr = &sa[0]
		if i == 0 {
			ai.ai_canonname = canonPtr
		}
		if prev != nil {
			prev.ai_next = ai
		}
		prev = ai
	}
	addrInfoMu.Lock()
	addrInfoBlocks[base] = blk
	addrInfoMu.Unlock()
	*(*uintptr)(unsafe.Pointer(res)) = base
	return 0
}

// resolveOffStack resolves node and service on a fresh goroutine, so Go's
// resolver (which starts goroutines and blocks on the network) never runs on a
// guest stack. The caller parks on the result like pthread_join does.
func resolveOffStack(node, service string, flags, family, socktype int32) (rc int, ips []net.IP, port uint16, canon string) {
	type result struct {
		rc    int
		ips   []net.IP
		port  uint16
		canon string
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		r.rc, r.ips, r.port, r.canon = resolve(node, service, flags, family, socktype)
		ch <- r
	}()
	r := <-ch
	return r.rc, r.ips, r.port, r.canon
}

func resolve(node, service string, flags, family, socktype int32) (int, []net.IP, uint16, string) {
	var port uint16
	if service != "" {
		if n, err := strconv.ParseUint(service, 10, 16); err == nil {
			port = uint16(n)
		} else if flags&aiNumericserv != 0 {
			return eaiNoname, nil, 0, ""
		} else {
			network := "tcp"
			if socktype == sockDgram {
				network = "udp"
			}
			p, err := goResolver.LookupPort(context.Background(), network, service)
			if err != nil {
				return eaiService, nil, 0, ""
			}
			port = uint16(p)
		}
	}
	var ips []net.IP
	canon := node
	switch {
	case node == "":
		// No node: the wildcard address with AI_PASSIVE, loopback otherwise.
		if flags&aiPassive != 0 {
			ips = []net.IP{net.IPv4zero, net.IPv6unspecified}
		} else {
			ips = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
		}
	case net.ParseIP(node) != nil:
		ips = []net.IP{net.ParseIP(node)}
	case flags&aiNumerichost != 0:
		return eaiNoname, nil, 0, ""
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		addrs, err := goResolver.LookupIPAddr(ctx, node)
		if err != nil {
			var de *net.DNSError
			if errors.As(err, &de) && (de.IsTemporary || de.IsTimeout) {
				return eaiAgain, nil, 0, ""
			}
			if errors.As(err, &de) && de.IsNotFound {
				return eaiNoname, nil, 0, ""
			}
			return eaiFail, nil, 0, ""
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		if flags&aiCanonname != 0 {
			if c, err := goResolver.LookupCNAME(ctx, node); err == nil {
				canon = strings.TrimSuffix(c, ".")
			}
		}
	}
	// Filter by family; IPv4 first, as bionic orders results on hosts without
	// IPv6 connectivity.
	var v4, v6 []net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			if family != afInet6 {
				v4 = append(v4, ip.To4())
			}
		} else if family != afInet {
			v6 = append(v6, ip)
		}
	}
	out := append(v4, v6...)
	if len(out) == 0 {
		return eaiNoname, nil, 0, ""
	}
	return 0, out, port, canon
}

//go:noinline
//go:nosplit
func goFreeaddrinfo(res uintptr) {
	addrInfoMu.Lock()
	delete(addrInfoBlocks, res)
	addrInfoMu.Unlock()
}

// goGaiStrerror reports bionic's EAI_* codes by name so the engine can log a
// useful failure instead of an opaque number.
//
//go:noinline
func goGaiStrerror(errcode int32) uintptr {
	names := map[int32]string{
		0:  "Success",
		1:  "Address family for hostname not supported",
		2:  "Temporary failure in name resolution",
		3:  "Invalid value for ai_flags",
		4:  "Non-recoverable failure in name resolution",
		5:  "ai_family not supported",
		6:  "Memory allocation failure",
		7:  "No address associated with hostname",
		8:  "hostname nor servname provided, or not known",
		9:  "servname not supported for ai_socktype",
		10: "ai_socktype not supported",
		11: "System error returned in errno",
		12: "Invalid value for hints",
		13: "Resolved protocol is unknown",
		14: "Argument buffer overflow",
	}
	if s, ok := names[errcode]; ok {
		return cstringConst(s)
	}
	return cstringConst("Unknown error")
}

// goInetNtop formats a binary address in the given family, like bionic's.
//
//go:noinline
func goInetNtop(af, src, dst, size uintptr) uintptr {
	if src == 0 || dst == 0 {
		return 0
	}
	var out string
	switch af {
	case afInet:
		p := unsafe.Slice((*byte)(unsafe.Pointer(src)), 4)
		out = net.IPv4(p[0], p[1], p[2], p[3]).String()
	case afInet6:
		out = net.IP(append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(src)), 16)...)).String()
	default:
		return 0
	}
	if uintptr(len(out))+1 > size {
		return 0
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(dst)), len(out)+1)
	copy(b, out)
	b[len(out)] = 0
	return dst
}

// sockaddrString renders an AF_INET/AF_INET6 sockaddr for diagnostics.
func sockaddrString(sa, salen uintptr) string {
	if sa == 0 || salen < 2 {
		return "<nil>"
	}
	p := unsafe.Slice((*byte)(unsafe.Pointer(sa)), int(salen))
	port := 0
	if len(p) >= 4 {
		port = int(binary.BigEndian.Uint16(p[2:4]))
	}
	switch binary.LittleEndian.Uint16(p[0:2]) {
	case afInet:
		if len(p) >= 8 {
			return fmt.Sprintf("%s:%d", net.IP(p[4:8]), port)
		}
	case afInet6:
		if len(p) >= 24 {
			return fmt.Sprintf("[%s]:%d", net.IP(p[8:24]), port)
		}
	}
	return fmt.Sprintf("family=%d", binary.LittleEndian.Uint16(p[0:2]))
}

// netFDs records which descriptors are sockets, so RBXRT_DEBUG_NET can trace
// socket traffic through the generic read/write/close calls too.
var netFDs sync.Map // fd -> struct{}

func isNetFD(fd uintptr) bool {
	_, ok := netFDs.Load(fd)
	return ok
}

// traceNet prints one socket operation when RBXRT_DEBUG_NET is set. A failure
// carries the thread's errno.
func traceNet(op string, fd uintptr, r int, extra string) {
	if !debugNet {
		return
	}
	if r < 0 {
		writeStderrRaw(fmt.Sprintf("rbxrt: net: %s(fd=%d%s) -> -1 errno=%d\n", op, fd, extra, *errnoPtr()))
		return
	}
	writeStderrRaw(fmt.Sprintf("rbxrt: net: %s(fd=%d%s) -> %d\n", op, fd, extra, r))
}

// netDumpMax bounds netBytes; RBXRT_DEBUG_NET=full dumps whole transfers.
var netDumpMax = func() int {
	if os.Getenv("RBXRT_DEBUG_NET") == "full" {
		return 1 << 16
	}
	return 16
}()

// netBytes renders the first bytes of a socket transfer for RBXRT_DEBUG_NET
// (TLS record headers and alerts are short enough to read whole).
func netBytes(buf uintptr, r int) string {
	if !debugNet || r <= 0 || buf == 0 {
		return ""
	}
	n := r
	if n > netDumpMax {
		n = netDumpMax
	}
	return fmt.Sprintf(" [% x]", unsafe.Slice((*byte)(unsafe.Pointer(buf)), n))
}
