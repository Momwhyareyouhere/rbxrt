package hostabi

import (
	"fmt"
	"os"
	"unsafe"
)

// This file implements the memory, string and numeric-conversion functions the
// bionic-compatible host exposes to guest code. Everything works on raw guest
// pointers (plain uintptr), so the implementations never create Go pointers to
// guest memory and never trigger the garbage collector.

//go:noinline
//go:nosplit
func goMemcpy(dst, src, n uintptr) uintptr {
	if n == 0 {
		return dst
	}
	d, s := dst, src
	for n >= 8 {
		*(*uint64)(unsafe.Pointer(d)) = *(*uint64)(unsafe.Pointer(s))
		d, s, n = d+8, s+8, n-8
	}
	for n > 0 {
		*(*byte)(unsafe.Pointer(d)) = *(*byte)(unsafe.Pointer(s))
		d, s, n = d+1, s+1, n-1
	}
	return dst
}

//go:noinline
//go:nosplit
func goMemmove(dst, src, n uintptr) uintptr {
	if n == 0 || dst == src {
		return dst
	}
	if dst < src {
		return goMemcpy(dst, src, n)
	}
	// backward copy
	d, s := dst+n, src+n
	for n > 0 {
		d--
		s--
		*(*byte)(unsafe.Pointer(d)) = *(*byte)(unsafe.Pointer(s))
		n--
	}
	return dst
}

//go:noinline
func goMemset(dst, c, n uintptr) uintptr {
	b := byte(c)
	d := dst
	// fill a word at a time when possible
	if n >= 8 {
		w := uint64(b) | uint64(b)<<8 | uint64(b)<<16 | uint64(b)<<24 |
			uint64(b)<<32 | uint64(b)<<40 | uint64(b)<<48 | uint64(b)<<56
		for n >= 8 {
			*(*uint64)(unsafe.Pointer(d)) = w
			d, n = d+8, n-8
		}
	}
	for n > 0 {
		*(*byte)(unsafe.Pointer(d)) = b
		d, n = d+1, n-1
	}
	return dst
}

//go:noinline
func goMemcmp(a, b, n uintptr) int {
	for n > 0 {
		x := *(*byte)(unsafe.Pointer(a))
		y := *(*byte)(unsafe.Pointer(b))
		if x != y {
			return int(x) - int(y)
		}
		a, b, n = a+1, b+1, n-1
	}
	return 0
}

//go:noinline
func goMemchr(s, c, n uintptr) uintptr {
	b := byte(c)
	for n > 0 {
		if *(*byte)(unsafe.Pointer(s)) == b {
			return s
		}
		s, n = s+1, n-1
	}
	return 0
}

//go:noinline
func goMemrchr(s, c, n uintptr) uintptr {
	b := byte(c)
	for n > 0 {
		n--
		if *(*byte)(unsafe.Pointer(s + n)) == b {
			return s + n
		}
	}
	return 0
}

//go:noinline
func goStrlen(s uintptr) uintptr {
	if s < 4096 {
		nullStringFault("strlen", s)
	}
	p := s
	for *(*byte)(unsafe.Pointer(p)) != 0 {
		p++
	}
	return p - s
}

//go:noinline
func goStrnlen(s, max uintptr) uintptr {
	p := s
	for max > 0 && *(*byte)(unsafe.Pointer(p)) != 0 {
		p++
		max--
	}
	return p - s
}

//go:noinline
func goStrcpy(dst, src uintptr) uintptr {
	ret := dst
	for {
		c := *(*byte)(unsafe.Pointer(src))
		*(*byte)(unsafe.Pointer(dst)) = c
		if c == 0 {
			return ret
		}
		dst, src = dst+1, src+1
	}
}

//go:noinline
func goStrncpy(dst, src, n uintptr) uintptr {
	ret := dst
	for n > 0 {
		c := *(*byte)(unsafe.Pointer(src))
		*(*byte)(unsafe.Pointer(dst)) = c
		dst, src, n = dst+1, src+1, n-1
		if c == 0 {
			// bionic pads the remainder with NULs
			for n > 0 {
				*(*byte)(unsafe.Pointer(dst)) = 0
				dst, n = dst+1, n-1
			}
			break
		}
	}
	return ret
}

//go:noinline
func goStrcat(dst, src uintptr) uintptr {
	ret := dst
	d := dst + goStrlen(dst)
	goStrcpy(d, src)
	return ret
}

//go:noinline
func goStrncat(dst, src, n uintptr) uintptr {
	ret := dst
	d := dst + goStrlen(dst)
	for n > 0 {
		c := *(*byte)(unsafe.Pointer(src))
		if c == 0 {
			break
		}
		*(*byte)(unsafe.Pointer(d)) = c
		d, src, n = d+1, src+1, n-1
	}
	*(*byte)(unsafe.Pointer(d)) = 0
	return ret
}

//go:noinline
func goStrcmp(a, b uintptr) int {
	if a < 4096 || b < 4096 {
		nullStringFault("strcmp", min(a, b))
	}
	for {
		x := *(*byte)(unsafe.Pointer(a))
		y := *(*byte)(unsafe.Pointer(b))
		if x != y {
			return int(x) - int(y)
		}
		if x == 0 {
			return 0
		}
		a, b = a+1, b+1
	}
}

//go:noinline
func goStrncmp(a, b, n uintptr) int {
	if n > 0 && (a < 4096 || b < 4096) {
		nullStringFault("strncmp", min(a, b))
	}
	for n > 0 {
		x := *(*byte)(unsafe.Pointer(a))
		y := *(*byte)(unsafe.Pointer(b))
		if x != y {
			return int(x) - int(y)
		}
		if x == 0 {
			return 0
		}
		a, b, n = a+1, b+1, n-1
	}
	return 0
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

//go:noinline
func goStrcasecmp(a, b uintptr) int {
	for {
		x := lower(*(*byte)(unsafe.Pointer(a)))
		y := lower(*(*byte)(unsafe.Pointer(b)))
		if x != y {
			return int(x) - int(y)
		}
		if x == 0 {
			return 0
		}
		a, b = a+1, b+1
	}
}

//go:noinline
func goStrncasecmp(a, b, n uintptr) int {
	for n > 0 {
		x := lower(*(*byte)(unsafe.Pointer(a)))
		y := lower(*(*byte)(unsafe.Pointer(b)))
		if x != y {
			return int(x) - int(y)
		}
		if x == 0 {
			return 0
		}
		a, b, n = a+1, b+1, n-1
	}
	return 0
}

//go:noinline
func goStrchr(s, c uintptr) uintptr {
	b := byte(c)
	for {
		x := *(*byte)(unsafe.Pointer(s))
		if x == b {
			return s
		}
		if x == 0 {
			return 0
		}
		s++
	}
}

//go:noinline
func goStrrchr(s, c uintptr) uintptr {
	b := byte(c)
	var found uintptr
	for {
		x := *(*byte)(unsafe.Pointer(s))
		if x == b {
			found = s
		}
		if x == 0 {
			return found
		}
		s++
	}
}

// cstrIs reports whether p points at a NUL-terminated string equal to s.
//
//go:noinline
func cstrIs(p uintptr, s string) bool {
	for i := 0; i < len(s); i++ {
		if *(*byte)(unsafe.Pointer(p + uintptr(i))) != s[i] {
			return false
		}
	}
	return *(*byte)(unsafe.Pointer(p + uintptr(len(s)))) == 0
}

// cstrN reads at most max bytes of the NUL-terminated string at p.
func cstrN(p uintptr, max int) string {
	b := make([]byte, 0, 64)
	for i := 0; i < max; i++ {
		c := *(*byte)(unsafe.Pointer(p + uintptr(i)))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

func goStrstr(hay, needle uintptr) uintptr {
	// The engine's fatal-error path asks whether its message mentions
	// "out of memory" before deciding how to report it. That report goes through
	// telemetry, which the runtime does not implement yet, so the message is
	// otherwise invisible; surface it here instead.
	if cstrIs(needle, "out of memory") {
		rp, _ := guestCapture()
		fmt.Fprintf(os.Stderr, "rbxrt: fatal message: %q caller=%s\n", cstrN(hay, 512), guestAddr(rp))
		if debugWalk {
			walkGuestFrames(24)
		}
	}
	nl := goStrlen(needle)
	if nl == 0 {
		return hay
	}
	for {
		f := *(*byte)(unsafe.Pointer(hay))
		if f == 0 {
			return 0
		}
		if f == *(*byte)(unsafe.Pointer(needle)) && goStrncmp(hay, needle, nl) == 0 {
			return hay
		}
		hay++
	}
}

//go:noinline
func goStrcspn(s, reject uintptr) uintptr {
	n := uintptr(0)
	for {
		c := *(*byte)(unsafe.Pointer(s + n))
		if c == 0 {
			return n
		}
		if goStrchr(reject, uintptr(c)) != 0 {
			return n
		}
		n++
	}
}

//go:noinline
func goStrspn(s, accept uintptr) uintptr {
	n := uintptr(0)
	for {
		c := *(*byte)(unsafe.Pointer(s + n))
		if c == 0 || goStrchr(accept, uintptr(c)) == 0 {
			return n
		}
		n++
	}
}

//go:noinline
func goStrpbrk(s, accept uintptr) uintptr {
	for {
		c := *(*byte)(unsafe.Pointer(s))
		if c == 0 {
			return 0
		}
		if goStrchr(accept, uintptr(c)) != 0 {
			return s
		}
		s++
	}
}

//go:noinline
func goStrcmpNull(a, b uintptr) int {
	if a == 0 && b == 0 {
		return 0
	}
	if a == 0 {
		return -1
	}
	if b == 0 {
		return 1
	}
	return goStrcmp(a, b)
}

// --- numeric conversions -------------------------------------------------

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func digitVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	}
	return 99
}

// goStrtox is the shared implementation of strtol/strtoll/strtoul/strtoull.
// It returns the converted value and, if endp != 0, stores the address of the
// first unconsumed character there.
func goStrtox(nptr, endp uintptr, base int, unsigned bool) uint64 {
	s := nptr
	for isSpace(*(*byte)(unsafe.Pointer(s))) {
		s++
	}
	neg := false
	if c := *(*byte)(unsafe.Pointer(s)); c == '+' || c == '-' {
		neg = c == '-'
		s++
	}
	if base == 0 {
		if *(*byte)(unsafe.Pointer(s)) == '0' {
			if c := *(*byte)(unsafe.Pointer(s + 1)); c == 'x' || c == 'X' {
				base = 16
				s += 2
			} else {
				base = 8
			}
		} else {
			base = 10
		}
	} else if base == 16 {
		if *(*byte)(unsafe.Pointer(s)) == '0' {
			if c := *(*byte)(unsafe.Pointer(s + 1)); c == 'x' || c == 'X' {
				s += 2
			}
		}
	}
	var val uint64
	any := false
	for {
		d := digitVal(*(*byte)(unsafe.Pointer(s)))
		if d >= base {
			break
		}
		any = true
		val = val*uint64(base) + uint64(d)
		s++
	}
	if !any {
		s = nptr
	}
	if endp != 0 {
		*(*uintptr)(unsafe.Pointer(endp)) = s
	}
	if neg {
		val = -val
	}
	return val
}

//go:noinline
func goAtoi(s uintptr) int {
	return int(int64(goStrtox(s, 0, 10, false)))
}

// nullStringFault reports a C string function handed a null (or near-null)
// pointer and aborts, naming the guest call site. bionic would fault here too;
// the report replaces an anonymous Go nil-dereference panic.
func nullStringFault(fn string, s uintptr) {
	rp, _ := guestCapture()
	writeStderrRaw(fmt.Sprintf("rbxrt: %s(%#x) called with a null pointer from %s\n", fn, s, guestAddr(rp)))
	if debugWalk {
		walkGuestFrames(24)
	}
	exitNow(139)
}
