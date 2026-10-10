package hostabi

import "unsafe"

// --- wide characters -----------------------------------------------------

//go:noinline
func goWcslen(s uintptr) uintptr {
	p := s
	for *(*uint32)(unsafe.Pointer(p)) != 0 {
		p += 4
	}
	return (p - s) / 4
}

//go:noinline
func goWmemchr(s, c, n uintptr) uintptr {
	w := uint32(c)
	for n > 0 {
		if *(*uint32)(unsafe.Pointer(s)) == w {
			return s
		}
		s, n = s+4, n-1
	}
	return 0
}

//go:noinline
func goWmemcmp(a, b, n uintptr) int {
	for n > 0 {
		x := *(*uint32)(unsafe.Pointer(a))
		y := *(*uint32)(unsafe.Pointer(b))
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
		a, b, n = a+4, b+4, n-1
	}
	return 0
}

// --- strtox wrappers -----------------------------------------------------

//go:noinline
func goStrtol(nptr, endp, base uintptr) uintptr {
	return uintptr(int64(goStrtox(nptr, endp, int(base), false)))
}

//go:noinline
func goStrtoul(nptr, endp, base uintptr) uintptr {
	return uintptr(goStrtox(nptr, endp, int(base), true))
}

//go:noinline
func goAtol(s uintptr) uintptr { return goStrtol(s, 0, 10) }

// --- fortify (_chk) variants ---------------------------------------------
//
// bionic's fortified interfaces take an extra buffer-size argument and abort on
// overflow. We honour that contract instead of silently ignoring the bound.

func fortifyFail(fn string) {
	writeLog("rbxrt", "buffer overflow detected in "+fn+" (fortify check failed)")
	exitNow(134)
}

//go:noinline
func goMemcpyChk(dst, src, n, dstlen uintptr) uintptr {
	if n > dstlen {
		fortifyFail("__memcpy_chk")
	}
	return goMemcpy(dst, src, n)
}

//go:noinline
func goMemmoveChk(dst, src, n, dstlen uintptr) uintptr {
	if n > dstlen {
		fortifyFail("__memmove_chk")
	}
	return goMemmove(dst, src, n)
}

//go:noinline
func goMemsetChk(dst, c, n, dstlen uintptr) uintptr {
	if n > dstlen {
		fortifyFail("__memset_chk")
	}
	return goMemset(dst, c, n)
}

//go:noinline
func goStrcpyChk(dst, src, dstlen uintptr) uintptr {
	if goStrlen(src) >= dstlen {
		fortifyFail("__strcpy_chk")
	}
	return goStrcpy(dst, src)
}

//go:noinline
func goStrncpyChk(dst, src, n, dstlen uintptr) uintptr {
	if n > dstlen {
		fortifyFail("__strncpy_chk")
	}
	return goStrncpy(dst, src, n)
}

//go:noinline
func goStrcatChk(dst, src, dstlen uintptr) uintptr {
	if goStrlen(dst)+goStrlen(src) >= dstlen {
		fortifyFail("__strcat_chk")
	}
	return goStrcat(dst, src)
}

//go:noinline
func goStrchrChk(s, c, slen uintptr) uintptr { return goStrchr(s, c) }

//go:noinline
func goStrlenChk(s, slen uintptr) uintptr { return goStrlen(s) }

//go:noinline
func goReadChk(fd, buf, n, buflen uintptr) int {
	if n > buflen {
		fortifyFail("__read_chk")
	}
	return goRead(fd, buf, n)
}

//go:noinline
func goWriteChk(fd, buf, n, buflen uintptr) int {
	if n > buflen {
		fortifyFail("__write_chk")
	}
	return goWrite(fd, buf, n)
}

//go:noinline
func goFdClrChk(fd, set uintptr) int   { return goFdClr(fd, set) }
func goFdIssetChk(fd, set uintptr) int { return goFdIsset(fd, set) }
func goFdSetChk(fd, set uintptr) int   { return goFdSet(fd, set) }

//go:noinline
func goFdClr(fd, set uintptr) int {
	p := (*uint64)(unsafe.Pointer(set + (fd/64)*8))
	*p &^= 1 << (fd % 64)
	return 0
}

//go:noinline
func goFdIsset(fd, set uintptr) int {
	p := (*uint64)(unsafe.Pointer(set + (fd/64)*8))
	if *p&(1<<(fd%64)) != 0 {
		return 1
	}
	return 0
}

//go:noinline
func goFdSet(fd, set uintptr) int {
	p := (*uint64)(unsafe.Pointer(set + (fd/64)*8))
	*p |= 1 << (fd % 64)
	return 0
}
