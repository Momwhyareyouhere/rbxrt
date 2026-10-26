package hostabi

import (
	"encoding/binary"
	"net"
	"strconv"
	"sync"
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
		errnoVar = 97 // EAFNOSUPPORT
		return -1
	}
	return 1
}

// goGetnameinfo implements the numeric and local-address cases of bionic's
// getnameinfo. For nonnumeric reverse lookups, it follows NI_NAMEREQD rather
// than pretending that a hostname was resolved; DNS support is supplied by the
// separate resolver interface when needed.
//
// C getnameinfo has seven integer/pointer arguments. mkThunk forwards the
// seventh stack argument into Go's seventh integer argument register.
//
//go:noinline
func goGetnameinfo(sa, salen, host, hostlen, serv, servlen, flags uintptr) int {
	if sa == 0 || salen < 2 {
		return -6 // EAI_FAMILY
	}
	family := *(*uint16)(unsafe.Pointer(sa))
	var hostname, service string
	switch family {
	case 2: // AF_INET
		if salen < 16 {
			return -6
		}
		p := unsafe.Slice((*byte)(unsafe.Pointer(sa)), 16)
		hostname = net.IPv4(p[4], p[5], p[6], p[7]).String()
		service = strconv.Itoa(int(binary.BigEndian.Uint16(p[2:4])))
	case 10: // AF_INET6
		if salen < 28 {
			return -6
		}
		p := unsafe.Slice((*byte)(unsafe.Pointer(sa)), int(salen))
		hostname = net.IP(p[8:24]).String()
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
		return -6
	}
	const niNameReq = 8
	if flags&niNameReq != 0 && flags&1 == 0 && family != 1 {
		return -2 // EAI_NONAME: no reverse resolver result available
	}
	if host != 0 && hostlen != 0 {
		if uintptr(len(hostname))+1 > hostlen {
			return -12
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(host)), len(hostname)), hostname)
		*(*byte)(unsafe.Pointer(host + uintptr(len(hostname)))) = 0
	}
	if serv != 0 && servlen != 0 {
		if uintptr(len(service))+1 > servlen {
			return -12
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(serv)), len(service)), service)
		*(*byte)(unsafe.Pointer(serv + uintptr(len(service)))) = 0
	}
	return 0
}

// minimal addrinfo structures for getaddrinfo.
//
// bionic's struct addrinfo is 56 bytes: six words of scalars plus three
// pointers, followed by the sockaddr_in/sockaddr_in6 area the engine expects
// at offset 56. The pool blocks below carry both.
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

// addrInfoPool hands out fixed 80-byte blocks (one addrinfo plus its
// sockaddr). They are kept forever: the engine frees them with
// freeaddrinfo, and a handful of lookups is far below any memory concern.
var (
	addrInfoMu  sync.Mutex
	addrInfoAll = [][]byte{}
)

// allocAddrInfo returns a block holding a zeroed addrinfo at block+0 and a
// sockaddr at block+56.
func allocAddrInfo() (*addrinfo, unsafe.Pointer, bool) {
	addrInfoMu.Lock()
	defer addrInfoMu.Unlock()
	buf := make([]byte, 80)
	addrInfoAll = append(addrInfoAll, buf)
	ai := (*addrinfo)(unsafe.Pointer(&buf[0]))
	return ai, unsafe.Pointer(&buf[56]), true
}

// goGetaddrinfo answers the engine's numeric lookups (e.g. loopback) without a
// resolver. It builds an addrinfo result by hand; anything it cannot answer
// comes back as EAI_NONAME so callers can fail forward, and a host with
// NI_NAMEREQD-style expectations is not invented.
//
//go:noinline
//go:nosplit
func goGetaddrinfo(node, service, hints, res uintptr) int {
	if res == 0 {
		return -1
	}
	*(*uintptr)(unsafe.Pointer(res)) = 0
	if node == 0 {
		return -5 // EAI_NONAME
	}
	nodeStr := cstr(node)
	if nodeStr == "" || nodeStr == "localhost" {
		nodeStr = "127.0.0.1"
	}
	ip := net.ParseIP(nodeStr)
	if ip == nil {
		return -5
	}
	var family uint16
	if ip.To4() != nil {
		family = 2
	} else {
		family = 10
	}
	// 32 bytes for the family/serv structs, 16+ for the sockaddr, 24 for
	// the addrinfo, and room for the canonical name.
	ai, sa, ok := allocAddrInfo()
	if !ok {
		return -11 // EAI_MEMORY
	}
	ai.ai_family = int32(family)
	ai.ai_socktype = 1 // SOCK_STREAM
	b := unsafe.Slice((*byte)(sa), 28)
	switch family {
	case 2:
		binary.BigEndian.PutUint16(b[0:2], 0)
		binary.BigEndian.PutUint16(b[2:4], 80)
		binary.LittleEndian.PutUint16(b[4:6], 2)
		copy(b[8:12], ip.To4())
	case 10:
		binary.BigEndian.PutUint16(b[0:2], 0)
		binary.BigEndian.PutUint16(b[2:4], 80)
		binary.LittleEndian.PutUint16(b[4:6], 10)
		copy(b[8:24], ip.To16())
	}
	if ip.To4() != nil {
		ai.ai_addrlen = 16
	} else {
		ai.ai_addrlen = 28
	}
	ai.ai_addr = (*byte)(sa)
	*(*uintptr)(unsafe.Pointer(res)) = uintptr(unsafe.Pointer(ai))
	return 0
}

//go:noinline
func goFreeaddrinfo(res uintptr) {
	// Nothing to free: the pool keeps blocks alive for the process.
}

// goGaiStrerror reports bionic's EAI_* codes by name so the engine can log a
// useful failure instead of an opaque number.
//
//go:noinline
func goGaiStrerror(errcode int32) uintptr {
	names := map[int32]string{
		-1:  "Success",
		-2:  "System error",
		-3:  "Non-recoverable failure",
		-4:  "Memory allocation failure",
		-5:  "No address, or name, for the host",
		-6:  "Address family not supported",
		-7:  "Operation not supported",
		-8:  "System error",
		-9:  "Bad value for the AI_FLAGS field",
		-10: "Temporary failure in name resolution",
		-11: "Out of memory",
		-12: "Buffer too small",
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
	p := unsafe.Slice((*byte)(unsafe.Pointer(src)), 1)
	var out string
	switch af {
	case 2: // AF_INET
		if size < 16 {
			return 0
		}
		out = net.IPv4(p[0], p[1], p[2], p[3]).String()
	case 10: // AF_INET6
		if size < 28 {
			return 0
		}
		out = net.IP(unsafe.Slice((*byte)(unsafe.Pointer(src)), 16)).String()
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
