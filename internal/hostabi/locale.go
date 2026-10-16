package hostabi

import (
	"unicode/utf8"
	"unsafe"
)

// This file implements the C locale, character-classification and multibyte
// layers. The guest's C++ runtime (libc++) probes the locale during static
// initialization, so these must answer coherently rather than trap.
//
// Only the C locale is provided. Multibyte conversions are UTF-8, which is what
// bionic uses by default on Android and what the engine expects for text. A
// locale_t handle is an opaque non-NULL object with no meaningful contents; the
// functions that accept one behave exactly as their locale-less counterparts,
// which is correct for a single-locale system.

// --- opaque locale handles ----------------------------------------------

var localePins [][]byte

func newLocaleObj() uintptr {
	b := make([]byte, 64)
	localePins = append(localePins, b)
	return uintptr(unsafe.Pointer(&b[0]))
}

//go:noinline
func goNewlocale(mask, name, base uintptr) uintptr {
	// We provide a single, always-available locale, so any requested name that
	// is not empty maps onto it. Returning NULL here would be more "honest"
	// about unsupported names but routinely crashes callers that do not check.
	return newLocaleObj()
}

//go:noinline
func goFreelocale(loc uintptr) int { return 0 }

//go:noinline
func goDuplocale(loc uintptr) uintptr { return newLocaleObj() }

// currentLocale tracks the per-thread locale set by uselocale. The runtime is
// single-threaded, so one variable is sufficient.
var currentLocale uintptr

const lcGlobalLocale = ^uintptr(0) // (locale_t)-1

//go:noinline
func goUselocale(loc uintptr) uintptr {
	prev := currentLocale
	switch loc {
	case 0: // query the current locale
	case lcGlobalLocale:
		currentLocale = 0
	default:
		currentLocale = loc
	}
	if prev == 0 {
		return lcGlobalLocale
	}
	return prev
}

var localeNameC = cstringConst("C")

//go:noinline
func goSetlocale(category, name uintptr) uintptr {
	// setlocale(_, NULL) queries; we always report "C". A requested locale that
	// we do not actually implement still reports success with the C locale, so
	// initialization proceeds; the strings it drives remain ASCII.
	return localeNameC
}

// struct lconv, bionic layout: ten char* fields followed by fourteen chars.
var lconvStorage [96]byte
var lconvOnce bool

//go:noinline
func goLocaleconv() uintptr {
	if !lconvOnce {
		dot := cstringConst(".")
		empty := cstringConst("")
		setPtr := func(off uintptr, p uintptr) {
			*(*uintptr)(unsafe.Pointer(&lconvStorage[off])) = p
		}
		setPtr(0, dot)         // decimal_point
		setPtr(8, empty)       // thousands_sep
		setPtr(16, empty)      // grouping
		setPtr(24, empty)      // int_curr_symbol
		setPtr(32, empty)      // currency_symbol
		setPtr(40, empty)      // mon_decimal_point
		setPtr(48, empty)      // mon_thousands_sep
		setPtr(56, empty)      // mon_grouping
		setPtr(64, empty)      // positive_sign
		setPtr(72, empty)      // negative_sign
		lconvStorage[80] = 127 // CHAR_MAX for every numeric/format char
		for i := 81; i < 94; i++ {
			lconvStorage[i] = 127
		}
		lconvOnce = true
	}
	return uintptr(unsafe.Pointer(&lconvStorage[0]))
}

// --- collation (locale-parameterised) ------------------------------------

//go:noinline
func goStrcollL(a, b, loc uintptr) int { return goStrcmp(a, b) }

//go:noinline
func goStrxfrmL(dst, src, n, loc uintptr) uintptr {
	l := goStrlen(src)
	if n > 0 {
		m := l
		if m >= n {
			m = n - 1
		}
		goMemcpy(dst, src, m)
		*(*byte)(unsafe.Pointer(dst + m)) = 0
	}
	return l
}

//go:noinline
func goWcscollL(a, b, loc uintptr) int { return wcscmp(a, b) }

//go:noinline
func goWcsxfrmL(dst, src, n, loc uintptr) uintptr {
	l := goWcslen(src) // in wide characters
	if n > 0 {
		m := l
		if m >= n {
			m = n - 1
		}
		goMemmove(dst, src, m*4)
		*(*uint32)(unsafe.Pointer(dst + m*4)) = 0
	}
	return l
}

//go:noinline
func goStrtollL(nptr, endp, base, loc uintptr) uintptr {
	return uintptr(int64(goStrtox(nptr, endp, int(base), false)))
}

//go:noinline
func goStrtoullL(nptr, endp, base, loc uintptr) uintptr {
	return uintptr(goStrtox(nptr, endp, int(base), true))
}

// --- character classification --------------------------------------------
//
// The plain functions receive the argument in EDI as an int; values may carry
// EOF (-1) or a byte. The wide and *_l variants receive a wint_t/wchar_t.

func isalphaC(c uint32) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isdigitC(c uint32) bool { return c >= '0' && c <= '9' }
func isalnumC(c uint32) bool { return isalphaC(c) || isdigitC(c) }
func isspaceC(c uint32) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
func isupperC(c uint32) bool { return c >= 'A' && c <= 'Z' }
func islowerC(c uint32) bool { return c >= 'a' && c <= 'z' }
func isxdigitC(c uint32) bool {
	return isdigitC(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func isprintC(c uint32) bool { return c >= 0x20 && c < 0x7f }
func isgraphC(c uint32) bool { return c > 0x20 && c < 0x7f }
func iscntrlC(c uint32) bool { return c < 0x20 || c == 0x7f }
func ispunctC(c uint32) bool { return isgraphC(c) && !isalnumC(c) }
func isblankC(c uint32) bool { return c == ' ' || c == '\t' }
func tolowerC(c uint32) uint32 {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}
func toupperC(c uint32) uint32 {
	if c >= 'a' && c <= 'z' {
		return c - 32
	}
	return c
}

//go:noinline
func goIsspace(c uintptr) int {
	if isspaceC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsalpha(c uintptr) int {
	if isalphaC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsdigit(c uintptr) int {
	if isdigitC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsalnum(c uintptr) int {
	if isalnumC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsupper(c uintptr) int {
	if isupperC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIslower(c uintptr) int {
	if islowerC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsxdigit(c uintptr) int {
	if isxdigitC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsprint(c uintptr) int {
	if isprintC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsgraph(c uintptr) int {
	if isgraphC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIscntrl(c uintptr) int {
	if iscntrlC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIspunct(c uintptr) int {
	if ispunctC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIsblank(c uintptr) int {
	if isblankC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goTolower(c uintptr) int {
	if int64(c) == -1 { // EOF
		return -1
	}
	return int(tolowerC(uint32(c)) & 0xff)
}

//go:noinline
func goToupper(c uintptr) int {
	if int64(c) == -1 { // EOF
		return -1
	}
	return int(toupperC(uint32(c)) & 0xff)
}

// Wide-character classification (ASCII + Latin-1 basics).
func iswalphaC(c uint32) bool {
	return isalphaC(c) || (c >= 0xC0 && c <= 0xFF && c != 0xD7 && c != 0xF7)
}
func iswdigitC(c uint32) bool { return isdigitC(c) }
func iswspaceC(c uint32) bool {
	return isspaceC(c) || c == 0x85 || c == 0xA0
}
func iswupperC(c uint32) bool { return isupperC(c) || (c >= 0xC0 && c <= 0xDE && c != 0xD7) }
func iswlowerC(c uint32) bool { return islowerC(c) || (c >= 0xE0 && c <= 0xFE && c != 0xF7) }

//go:noinline
func goIswalpha(c uintptr) int {
	if iswalphaC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswdigit(c uintptr) int {
	if iswdigitC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswspace(c uintptr) int {
	if iswspaceC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswprint(c uintptr) int {
	if isprintC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswblank(c uintptr) int {
	if isblankC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswcntrl(c uintptr) int {
	if iscntrlC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswupper(c uintptr) int {
	if iswupperC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswlower(c uintptr) int {
	if iswlowerC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswpunct(c uintptr) int {
	if ispunctC(uint32(c)) {
		return 1
	}
	return 0
}

//go:noinline
func goIswxdigit(c uintptr) int {
	if isxdigitC(uint32(c)) {
		return 1
	}
	return 0
}

func towupperC(c uint32) uint32 {
	if c < 0x80 {
		return toupperC(c)
	}
	if c >= 0xE0 && c <= 0xFE && c != 0xF7 {
		return c - 32
	}
	return c
}
func towlowerC(c uint32) uint32 {
	if c < 0x80 {
		return tolowerC(c)
	}
	if c >= 0xC0 && c <= 0xDE && c != 0xD7 {
		return c + 32
	}
	return c
}

//go:noinline
func goTowupper(c uintptr) int { return int(towupperC(uint32(c))) }

//go:noinline
func goTowlower(c uintptr) int { return int(towlowerC(uint32(c))) }

// The *_l variants ignore the locale handle (one locale).
//
//go:noinline
func goIswalphaL(c, loc uintptr) int { return goIswalpha(c) }

//go:noinline
func goIswdigitL(c, loc uintptr) int { return goIswdigit(c) }

//go:noinline
func goIswspaceL(c, loc uintptr) int { return goIswspace(c) }

//go:noinline
func goIswprintL(c, loc uintptr) int { return goIswprint(c) }

//go:noinline
func goIswblankL(c, loc uintptr) int { return goIswblank(c) }

//go:noinline
func goIswcntrlL(c, loc uintptr) int { return goIswcntrl(c) }

//go:noinline
func goIswupperL(c, loc uintptr) int { return goIswupper(c) }

//go:noinline
func goIswlowerL(c, loc uintptr) int { return goIswlower(c) }

//go:noinline
func goIswpunctL(c, loc uintptr) int { return goIswpunct(c) }

//go:noinline
func goIswxdigitL(c, loc uintptr) int { return goIswxdigit(c) }

//go:noinline
func goTowupperL(c, loc uintptr) int { return int(towupperC(uint32(c))) }

//go:noinline
func goTowlowerL(c, loc uintptr) int { return int(towlowerC(uint32(c))) }

// --- multibyte <-> wide (UTF-8) ------------------------------------------
//
// Conversions read guest memory byte by byte; Go's unicode/utf8 does the
// decoding on a small local copy so no Go pointer ever refers to guest memory.

const (
	mbInvalid    = ^uintptr(0) // (size_t)-1
	mbIncomplete = ^uintptr(1) // (size_t)-2
)

// decodeMB decodes one UTF-8 sequence of at most n bytes at p. st is 0 on
// success, -1 for an invalid sequence, -2 if more bytes are needed.
func decodeMB(p, n uintptr) (r rune, size uintptr, st int) {
	var buf [4]byte
	m := n
	if m > 4 {
		m = 4
	}
	for i := uintptr(0); i < m; i++ {
		buf[i] = *(*byte)(unsafe.Pointer(p + i))
	}
	r, sz := utf8.DecodeRune(buf[:m])
	size = uintptr(sz)
	if r == utf8.RuneError && size <= 1 {
		b0 := buf[0]
		need := 0
		switch {
		case b0 >= 0xC2 && b0 <= 0xDF:
			need = 2
		case b0 >= 0xE0 && b0 <= 0xEF:
			need = 3
		case b0 >= 0xF0 && b0 <= 0xF4:
			need = 4
		}
		if need > int(n) {
			return 0, 0, -2
		}
		return 0, 0, -1
	}
	return r, size, 0
}

//go:noinline
func goMbrtowc(pwc, s, n, ps uintptr) uintptr {
	if s == 0 {
		return 0
	}
	if n == 0 {
		return mbIncomplete
	}
	r, size, st := decodeMB(s, n)
	if st == -2 {
		return mbIncomplete
	}
	if st == -1 {
		return mbInvalid
	}
	if pwc != 0 {
		*(*uint32)(unsafe.Pointer(pwc)) = uint32(r)
	}
	if r == 0 {
		return 0
	}
	return size
}

//go:noinline
func goMbtowc(pwc, s, n uintptr) int {
	if s == 0 {
		return 0
	}
	r, size, st := decodeMB(s, n)
	if st != 0 {
		return -1
	}
	if pwc != 0 {
		*(*uint32)(unsafe.Pointer(pwc)) = uint32(r)
	}
	if r == 0 {
		return 0
	}
	return int(size)
}

//go:noinline
func goMbrlen(s, n, ps uintptr) uintptr { return goMbrtowc(0, s, n, ps) }

//go:noinline
func goBtowc(c uintptr) int {
	if c == ^uintptr(0) { // EOF
		return -1
	}
	if c < 0x80 {
		return int(c)
	}
	return -1
}

//go:noinline
func goWctob(c uintptr) int {
	if c < 0x80 {
		return int(c)
	}
	return -1
}

//go:noinline
func goCtypeGetMbCurMax() int { return 4 }

func encodeMB(dst uintptr, r rune) uintptr {
	var buf [4]byte
	n := utf8.EncodeRune(buf[:], r)
	for i := uintptr(0); i < uintptr(n); i++ {
		*(*byte)(unsafe.Pointer(dst + i)) = buf[i]
	}
	return uintptr(n)
}

//go:noinline
func goWcrtomb(s uintptr, wc, ps uintptr) uintptr {
	if s == 0 {
		return 1
	}
	return encodeMB(s, rune(int32(wc)))
}

//go:noinline
func goMbsrtowcs(dst, srcp, len, ps uintptr) uintptr {
	if srcp == 0 {
		return 0
	}
	src := *(*uintptr)(unsafe.Pointer(srcp))
	count := uintptr(0)
	for {
		r, size, st := decodeMB(src, 4)
		if st == -2 || st == -1 {
			return mbInvalid
		}
		if r == 0 {
			break
		}
		if dst != 0 {
			if count >= len {
				break
			}
			*(*uint32)(unsafe.Pointer(dst + count*4)) = uint32(r)
		}
		count++
		src += size
	}
	if dst != 0 {
		if count < len || len == 0 {
			*(*uint32)(unsafe.Pointer(dst + count*4)) = 0
		}
		if count < len {
			*(*uintptr)(unsafe.Pointer(srcp)) = 0
		}
	}
	return count
}

//go:noinline
func goMbsnrtowcs(dst, srcp, nms, len, ps uintptr) uintptr {
	if srcp == 0 {
		return 0
	}
	src := *(*uintptr)(unsafe.Pointer(srcp))
	count := uintptr(0)
	consumed := uintptr(0)
	for consumed < nms {
		r, size, st := decodeMB(src, 4)
		if st == -2 || st == -1 {
			return mbInvalid
		}
		if r == 0 {
			src = 0
			*(*uintptr)(unsafe.Pointer(srcp)) = 0
			return count
		}
		if dst != 0 && count >= len {
			*(*uintptr)(unsafe.Pointer(srcp)) = src
			return count
		}
		if dst != 0 {
			*(*uint32)(unsafe.Pointer(dst + count*4)) = uint32(r)
		}
		count++
		src += size
		consumed += size
	}
	*(*uintptr)(unsafe.Pointer(srcp)) = src
	return count
}

//go:noinline
func goWcsrtombs(dst, srcp, len, ps uintptr) uintptr {
	if srcp == 0 {
		return 0
	}
	src := *(*uintptr)(unsafe.Pointer(srcp))
	count := uintptr(0)
	for {
		wc := *(*uint32)(unsafe.Pointer(src))
		if wc == 0 {
			break
		}
		var tmp [4]byte
		n := utf8.EncodeRune(tmp[:], rune(int32(wc)))
		if dst != 0 {
			if count+uintptr(n) > len {
				*(*uintptr)(unsafe.Pointer(srcp)) = src
				return count
			}
			for i := 0; i < n; i++ {
				*(*byte)(unsafe.Pointer(dst + count + uintptr(i))) = tmp[i]
			}
		}
		count += uintptr(n)
		src += 4
	}
	if dst != 0 {
		if count < len {
			*(*byte)(unsafe.Pointer(dst + count)) = 0
		}
		*(*uintptr)(unsafe.Pointer(srcp)) = 0
	}
	return count
}

//go:noinline
func goWcsnrtombs(dst, srcp, nwc, len, ps uintptr) uintptr {
	if srcp == 0 {
		return 0
	}
	src := *(*uintptr)(unsafe.Pointer(srcp))
	count := uintptr(0)
	var i uintptr
	for i = 0; i < nwc; i++ {
		wc := *(*uint32)(unsafe.Pointer(src + i*4))
		if wc == 0 {
			src = 0
			*(*uintptr)(unsafe.Pointer(srcp)) = 0
			return count
		}
		var tmp [4]byte
		n := utf8.EncodeRune(tmp[:], rune(int32(wc)))
		if dst != 0 && count+uintptr(n) > len {
			*(*uintptr)(unsafe.Pointer(srcp)) = src + i*4
			return count
		}
		if dst != 0 {
			for j := 0; j < n; j++ {
				*(*byte)(unsafe.Pointer(dst + count + uintptr(j))) = tmp[j]
			}
		}
		count += uintptr(n)
	}
	*(*uintptr)(unsafe.Pointer(srcp)) = src + i*4
	return count
}
