package hostabi

import (
	"strconv"
	"unsafe"
)

// scanf. The engine reads configuration/version strings with sscanf far more
// often than binary input, so the targets are small: integer, string, float and
// width-limited conversions, with the standard pointer targets consumed from
// the guest's va_list.

// scanSrc is the input abstraction: either an in-memory string (sscanf) or a
// FILE stream (fscanf).
type scanSrc struct {
	s   []byte
	pos int
	cf  *cFile
	n   int // characters consumed total
}

func (s *scanSrc) getc() (byte, bool) {
	if s.cf != nil {
		var b [1]byte
		if fileRead(s.cf, b[:]) == 1 {
			s.n++
			return b[0], true
		}
		return 0, false
	}
	if s.pos < len(s.s) {
		b := s.s[s.pos]
		s.pos++
		s.n++
		return b, true
	}
	return 0, false
}

func (s *scanSrc) ungetc(b byte) {
	if s.cf != nil {
		s.cf.mu.Lock()
		s.cf.ungot = int(b)
		s.cf.mu.Unlock()
		s.n--
		return
	}
	if s.pos > 0 {
		s.pos--
		s.n--
	}
}

func isWSc(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

// skipWS consumes and discards any amount of whitespace.
func skipWS(src *scanSrc) (byte, bool) {
	for {
		ch, ok := src.getc()
		if !ok {
			return 0, false
		}
		if !isWSc(ch) {
			src.ungetc(ch)
			return ch, true
		}
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isXDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func hexVal(b byte) uint64 {
	switch {
	case b >= '0' && b <= '9':
		return uint64(b - '0')
	case b >= 'a' && b <= 'f':
		return uint64(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return uint64(b-'A') + 10
	}
	return 0
}

// scanInts reads an integer. width limits characters read (sign included).
// base<0 means %i (auto-detect 0x/0/dec). Returns false if nothing matched.
func scanInts(src *scanSrc, width, base int) (val int64, neg bool, ok bool) {
	if width == 0 {
		width = 1 << 30
	}
	// skipWS only peeks at the first non-space character; consume it.
	if _, okch := skipWS(src); !okch {
		return 0, false, false
	}
	ch, okch := src.getc()
	if !okch {
		return 0, false, false
	}
	neg = false
	if ch == '-' || ch == '+' {
		neg = ch == '-'
		width--
		ch, okch = src.getc()
		if !okch {
			return 0, neg, false
		}
	}
	auto := base < 0
	if auto {
		base = 10
	}
	// For %i and %x, detect a 0x/0X prefix.
	if (auto || base == 16) && ch == '0' && width >= 2 {
		a, ok2 := src.getc()
		if ok2 && (a == 'x' || a == 'X') {
			base = 16
			width -= 2
			ch, okch = src.getc()
			if !okch {
				return 0, neg, false
			}
		} else if ok2 {
			src.ungetc(a)
			if auto {
				base = 8
			}
		}
	}
	var dval uint64
	consumed := 0
	for {
		// The first character that is not a digit of this base ends the
		// number and is left unread for the rest of the format.
		d := -1
		switch {
		case ch >= '0' && ch <= '9':
			d = int(ch - '0')
		case ch >= 'a' && ch <= 'f':
			d = int(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			d = int(ch-'A') + 10
		}
		if d < 0 || d >= base {
			src.ungetc(ch)
			break
		}
		consumed++
		dval = dval*uint64(base) + uint64(d)
		width--
		if width <= 0 {
			break
		}
		nc, ok3 := src.getc()
		if !ok3 {
			break
		}
		ch = nc
	}
	if consumed == 0 {
		return 0, neg, false
	}
	if neg {
		return -int64(dval), true, true
	}
	return int64(dval), false, true
}

func storeInt(v *vaList, val uint64, signed, suppress bool, length int) {
	if suppress {
		return
	}
	p := uintptr(v.gp())
	if p == 0 {
		return
	}
	switch {
	case signed && (length == 1):
		*(*int16)(unsafe.Pointer(p)) = int16(int64(val))
	case signed && (length == 2):
		*(*int8)(unsafe.Pointer(p)) = int8(int64(val))
	case signed && (length == 3 || length == 4):
		*(*int64)(unsafe.Pointer(p)) = int64(val)
	case signed:
		*(*int32)(unsafe.Pointer(p)) = int32(int64(val))
	case length == 1:
		*(*uint16)(unsafe.Pointer(p)) = uint16(val)
	case length == 2:
		*(*uint8)(unsafe.Pointer(p)) = uint8(val)
	case length == 3 || length == 4:
		*(*uint64)(unsafe.Pointer(p)) = val
	default:
		*(*uint32)(unsafe.Pointer(p)) = uint32(val)
	}
}

// scanFloatVal reads a decimal float from the input source. Returns false if
// nothing matched.
func scanFloatVal(src *scanSrc, width int) (float64, bool) {
	if width == 0 {
		width = 1 << 30
	}
	// skipWS only peeks at the first non-space character; consume it.
	if _, ok := skipWS(src); !ok {
		return 0, false
	}
	ch, ok := src.getc()
	if !ok {
		return 0, false
	}
	var tok []byte
	for width > 0 {
		accept := false
		switch {
		case ch >= '0' && ch <= '9':
			accept = true
		case (ch == '-' || ch == '+') && len(tok) == 0:
			accept = true
		case ch == '.':
			accept = true // even "1." or ".5"
		case ch == 'e' || ch == 'E':
			accept = true
		case (ch == '-' || ch == '+') && len(tok) > 0 &&
			(tok[len(tok)-1] == 'e' || tok[len(tok)-1] == 'E'):
			accept = true
		}
		if !accept {
			src.ungetc(ch)
			break
		}
		tok = append(tok, ch)
		width--
		if width > 0 {
			nc, ok2 := src.getc()
			if !ok2 {
				break
			}
			ch = nc
		} else {
			break
		}
	}
	if len(tok) == 0 {
		return 0, false
	}
	fl, err := strconv.ParseFloat(string(tok), 64)
	if err != nil {
		return 0, false
	}
	return fl, true
}

// storeFloat writes a scanned float per the length modifier.
func storeFloat(v *vaList, fl float64, suppress bool, length int) {
	if suppress {
		return
	}
	p := uintptr(v.gp())
	if p == 0 {
		return
	}
	if length == 3 || length == 5 {
		*(*float64)(unsafe.Pointer(p)) = fl
	} else {
		*(*float32)(unsafe.Pointer(p)) = float32(fl)
	}
}

// scanString reads a whitespace-delimited token and stores it NUL-terminated.
func scanString(src *scanSrc, width int, suppress bool, v *vaList) (bool, bool) {
	if width == 0 {
		width = 1 << 30
	}
	if _, ok := skipWS(src); !ok {
		return false, false
	}
	var out []byte
	for width > 0 {
		ch, ok := src.getc()
		if !ok {
			break
		}
		if isWSc(ch) {
			src.ungetc(ch)
			break
		}
		out = append(out, ch)
		width--
	}
	if len(out) == 0 {
		return false, false
	}
	if !suppress {
		p := uintptr(v.gp())
		if p != 0 {
			copyToGuest(p, out)
			*(*byte)(unsafe.Pointer(p + uintptr(len(out)))) = 0
		}
	}
	return true, true
}

// parseBracketSet handles "%[...]" after the '['. Returns the set and complement flag.
func parseBracketSet(fmt string, i int) (set []bool, negate bool, next int, ok bool) {
	set = make([]bool, 256)
	if i < len(fmt) && fmt[i] == '^' {
		negate = true
		i++
	}
	if i < len(fmt) && fmt[i] == ']' {
		set[']'] = true
		i++
	}
	got := false
	for i < len(fmt) && fmt[i] != ']' {
		set[fmt[i]] = true
		got = true
		i++
	}
	if !got || i >= len(fmt) {
		return nil, false, 0, false
	}
	return set, negate, i + 1, true
}

// scanfCore parses fmt against src, consuming assignment targets from v.
func scanfCore(fmtv string, src *scanSrc, v *vaList) int {
	assigned := 0
	i := 0
	for i < len(fmtv) {
		c := fmtv[i]
		if isWSc(c) {
			skipWS(src)
			i++
			continue
		}
		if c != '%' {
			ch, ok := src.getc()
			if !ok {
				return assigned
			}
			if ch != c {
				src.ungetc(ch)
				return assigned
			}
			i++
			continue
		}
		i++
		if i >= len(fmtv) {
			return assigned
		}
		if fmtv[i] == '%' {
			ch, ok := src.getc()
			if !ok || ch != '%' {
				if ok {
					src.ungetc(ch)
				}
				return assigned
			}
			i++
			continue
		}
		suppress := false
		if fmtv[i] == '*' {
			suppress = true
			i++
		}
		width := 0
		for i < len(fmtv) && fmtv[i] >= '0' && fmtv[i] <= '9' {
			width = width*10 + int(fmtv[i]-'0')
			i++
		}
		length := 0
		if i < len(fmtv) {
			switch fmtv[i] {
			case 'h':
				length = 1
				i++
				if i < len(fmtv) && fmtv[i] == 'h' {
					length = 2
					i++
				}
			case 'l':
				length = 3
				i++
				if i < len(fmtv) && fmtv[i] == 'l' {
					length = 4
					i++
				}
			case 'L':
				length = 5
				i++
			case 'z', 'j', 't':
				// size_t, intmax_t, ptrdiff_t: all 64-bit on LP64 bionic.
				// (/proc/self/statm is read with "%zu %zu %zu".)
				length = 3
				i++
			}
		}
		if i >= len(fmtv) {
			return assigned
		}
		conv := fmtv[i]
		i++

		switch conv {
		case 'd':
			val, _, ok := scanInts(src, width, 10)
			if !ok {
				return assigned
			}
			storeInt(v, uint64(val), true, suppress, length) // val is already signed
			if !suppress {
				assigned++
			}
		case 'i':
			val, _, ok := scanInts(src, width, -1)
			if !ok {
				return assigned
			}
			storeInt(v, uint64(val), true, suppress, length) // val is already signed
			if !suppress {
				assigned++
			}
		case 'u':
			val, _, ok := scanInts(src, width, 10)
			if !ok {
				return assigned
			}
			storeInt(v, uint64(val), false, suppress, length) // val is already signed
			if !suppress {
				assigned++
			}
		case 'x', 'X', 'p':
			val, _, ok := scanInts(src, width, 16)
			if !ok {
				return assigned
			}
			storeInt(v, uint64(val), false, suppress, length)
			if !suppress {
				assigned++
			}
		case 'o':
			val, _, ok := scanInts(src, width, 8)
			if !ok {
				return assigned
			}
			storeInt(v, uint64(val), false, suppress, length)
			if !suppress {
				assigned++
			}
		case 'f', 'e', 'g', 'a', 'F', 'E', 'G', 'A':
			fl, ok := scanFloatVal(src, width)
			if !ok {
				return assigned
			}
			storeFloat(v, fl, suppress, length)
			if !suppress {
				assigned++
			}
		case 's':
			_, ok := scanString(src, width, suppress, v)
			if !ok {
				return assigned
			}
			if !suppress {
				assigned++
			}
		case 'c':
			if width == 0 {
				width = 1
			}
			var out []byte
			for width > 0 {
				ch, ok := src.getc()
				if !ok {
					break
				}
				out = append(out, ch)
				width--
			}
			if len(out) == 0 {
				return assigned
			}
			if !suppress {
				p := uintptr(v.gp())
				if p != 0 {
					copyToGuest(p, out)
				}
			}
			if !suppress {
				assigned++
			}
		case '[':
			set, negate, next, ok := parseBracketSet(fmtv, i)
			if !ok {
				return assigned
			}
			i = next
			if _, ok := skipWS(src); !ok {
				return assigned
			}
			var out []byte
			for {
				ch, ok := src.getc()
				if !ok {
					break
				}
				if set[ch] == !negate {
					out = append(out, ch)
				} else {
					src.ungetc(ch)
					break
				}
			}
			if len(out) == 0 && !negate {
				return assigned
			}
			if !suppress {
				p := uintptr(v.gp())
				if p != 0 {
					copyToGuest(p, out)
					*(*byte)(unsafe.Pointer(p + uintptr(len(out)))) = 0
				}
			}
			if !suppress {
				assigned++
			}
		case 'n':
			if !suppress {
				p := uintptr(v.gp())
				if p != 0 {
					*(*int)(unsafe.Pointer(p)) = int(src.n)
				}
			}
		default:
			return assigned
		}
	}
	return assigned
}

// --- entry points ---------------------------------------------------------

// goSscanf implements sscanf(const char *str, const char *fmt, ...) — variadic,
// reached through goVariadic.
func runSscanf(strp, fmtp uintptr, v *vaList) int {
	src := &scanSrc{s: []byte(cstr(strp))}
	return scanfCore(cstr(fmtp), src, v)
}

// goFscanfEntry implements fscanf(FILE *stream, const char *fmt, ...).
func runFscanf(stream, fmtp uintptr, v *vaList) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	src := &scanSrc{cf: cf}
	return scanfCore(cstr(fmtp), src, v)
}

// goVsscanf implements vsscanf(const char *str, const char *fmt, va_list ap).
//
//go:noinline
func goVsscanf(str, format, ap uintptr) int {
	v := (*vaList)(unsafe.Pointer(ap))
	return runSscanf(str, format, v)
}
