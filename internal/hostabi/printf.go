package hostabi

import (
	"math"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

var bootPrintfMode = os.Getenv("RBXRT_BOOT") != ""

// --- C varargs on x86-64 -------------------------------------------------
//
// va_list is a pointer to this four-field descriptor (bionic and glibc agree).
// A guest that calls vsnprintf has already executed va_start, so the register
// save area and overflow area are complete and self-contained; the host only
// has to interpret them. This matters because the host thunk cannot capture a
// variadic guest's arguments itself: by the time vsnprintf is called the guest
// has spilled them into exactly this layout.
type vaList struct {
	gpOffset uint32 // byte offset of the next general-purpose slot in regSave
	fpOffset uint32 // byte offset of the next SSE slot in regSave
	overflow uintptr
	regSave  uintptr
}

// gp returns the next integer/pointer argument.
func (v *vaList) gp() uint64 {
	if v.gpOffset < 48 { // six GP registers are saved
		x := *(*uint64)(unsafe.Pointer(v.regSave + uintptr(v.gpOffset)))
		v.gpOffset += 8
		return x
	}
	x := *(*uint64)(unsafe.Pointer(v.overflow))
	v.overflow += 8
	return x
}

// fp returns the next double argument.
func (v *vaList) fp() float64 {
	if v.fpOffset < 176 { // eight 16-byte SSE slots
		x := *(*uint64)(unsafe.Pointer(v.regSave + uintptr(v.fpOffset)))
		v.fpOffset += 16
		return math.Float64frombits(x)
	}
	x := *(*uint64)(unsafe.Pointer(v.overflow))
	v.overflow += 8
	return math.Float64frombits(x)
}

// --- printf formatting ---------------------------------------------------

const (
	flagMinus = 1 << iota
	flagPlus
	flagSpace
	flagHash
	flagZero
)

const (
	lenNone = iota
	lenHH
	lenH
	lenL
	lenLL
	lenZ
	lenJ
	lenT
	lenBigL
)

// formatC renders a C printf format string, consuming arguments from v. Only
// the conversions the engine is expected to use are implemented; anything
// unrecognised is passed through verbatim, which is more useful than failing.
func formatC(format string, v *vaList) []byte {
	out := make([]byte, 0, len(format)+16)
	i := 0
	for i < len(format) {
		c := format[i]
		if c != '%' {
			out = append(out, c)
			i++
			continue
		}
		i++
		if i >= len(format) {
			out = append(out, '%')
			break
		}
		if format[i] == '%' {
			out = append(out, '%')
			i++
			continue
		}

		flags := 0
	flagsLoop:
		for i < len(format) {
			switch format[i] {
			case '-':
				flags |= flagMinus
			case '+':
				flags |= flagPlus
			case ' ':
				flags |= flagSpace
			case '#':
				flags |= flagHash
			case '0':
				flags |= flagZero
			default:
				break flagsLoop
			}
			i++
		}

		width := -1
		if i < len(format) && format[i] == '*' {
			width = int(int32(v.gp()))
			i++
		} else {
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				if width < 0 {
					width = 0
				}
				width = width*10 + int(format[i]-'0')
				i++
			}
			if width >= 0 && flags&flagZero != 0 {
				// A leading zero on a width is the flag, not part of the width.
			}
		}

		prec := -1
		if i < len(format) && format[i] == '.' {
			i++
			if i < len(format) && format[i] == '*' {
				prec = int(int32(v.gp()))
				i++
			} else {
				prec = 0
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					prec = prec*10 + int(format[i]-'0')
					i++
				}
			}
		}

		length := lenNone
	lenLoop:
		for i < len(format) {
			switch format[i] {
			case 'h':
				if length == lenH {
					length = lenHH
				} else {
					length = lenH
				}
			case 'l':
				if length == lenL {
					length = lenLL
				} else {
					length = lenL
				}
			case 'z':
				length = lenZ
			case 'j':
				length = lenJ
			case 't':
				length = lenT
			case 'L':
				length = lenBigL
			default:
				break lenLoop
			}
			i++
		}

		if i >= len(format) {
			out = append(out, '%')
			break
		}
		conv := format[i]
		i++

		switch conv {
		case 'd', 'i':
			x := v.gp()
			var val int64
			switch length {
			case lenL, lenLL, lenJ, lenT:
				val = int64(x)
			default:
				val = int64(int32(x))
			}
			neg := val < 0
			var mag uint64
			if neg {
				mag = uint64(-val)
			} else {
				mag = uint64(val)
			}
			sign := ""
			if neg {
				sign = "-"
			} else if flags&flagPlus != 0 {
				sign = "+"
			} else if flags&flagSpace != 0 {
				sign = " "
			}
			out = appendPadded(out, sign, "", strconv.FormatUint(mag, 10), width, prec, flags)

		case 'u', 'o', 'x', 'X':
			x := v.gp()
			var val uint64
			switch length {
			case lenL, lenLL, lenJ, lenT, lenZ:
				val = x
			default:
				val = uint64(uint32(x))
			}
			var digits, prefix string
			switch conv {
			case 'u':
				digits = strconv.FormatUint(val, 10)
			case 'o':
				digits = strconv.FormatUint(val, 8)
				if flags&flagHash != 0 && (len(digits) == 0 || digits[0] != '0') {
					prefix = "0"
				}
			case 'x':
				digits = strconv.FormatUint(val, 16)
				if flags&flagHash != 0 && val != 0 {
					prefix = "0x"
				}
			case 'X':
				digits = strings.ToUpper(strconv.FormatUint(val, 16))
				if flags&flagHash != 0 && val != 0 {
					prefix = "0X"
				}
			}
			out = appendPadded(out, "", prefix, digits, width, prec, flags)

		case 'c':
			ch := byte(v.gp())
			out = appendPadded(out, "", "", string([]byte{ch}), width, -1, flags&^flagZero)

		case 's':
			p := v.gp()
			s := cstr(uintptr(p))
			if p == 0 {
				s = "(null)"
			}
			if prec >= 0 && prec < len(s) {
				s = s[:prec]
			}
			out = appendPadded(out, "", "", s, width, -1, flags&^flagZero)

		case 'p':
			p := v.gp()
			if p == 0 {
				out = appendPadded(out, "", "", "(nil)", width, -1, flags&^flagZero)
			} else {
				out = appendPadded(out, "", "0x", strconv.FormatUint(p, 16), width, -1, flags&^flagZero)
			}

		case 'n':
			p := v.gp()
			if p != 0 {
				*(*int32)(unsafe.Pointer(uintptr(p))) = int32(len(out))
			}

		case 'f', 'F', 'e', 'E', 'g', 'G', 'a', 'A':
			x := v.fp()
			sign := ""
			if math.Signbit(x) {
				sign = "-"
				x = -x
			} else if flags&flagPlus != 0 {
				sign = "+"
			} else if flags&flagSpace != 0 {
				sign = " "
			}
			p := prec
			if p < 0 {
				p = 6
			}
			var body string
			switch conv {
			case 'f', 'F':
				body = strconv.FormatFloat(x, 'f', p, 64)
			case 'e', 'E':
				body = strconv.FormatFloat(x, 'e', p, 64)
			case 'g', 'G':
				body = strconv.FormatFloat(x, 'g', p, 64)
			case 'a', 'A':
				body = strconv.FormatFloat(x, 'x', p, 64)
			}
			if conv == 'F' || conv == 'E' || conv == 'G' || conv == 'A' {
				body = strings.ToUpper(body)
			}
			out = appendPadded(out, sign, "", body, width, -1, flags)

		default:
			out = append(out, '%')
			out = append(out, conv)
		}
	}
	return out
}

// appendPadded assembles sign + prefix + digits with width and precision,
// honouring left-justification and zero padding. Zero padding is suppressed by
// an explicit precision, matching C.
func appendPadded(out []byte, sign, prefix, digits string, width, prec, flags int) []byte {
	if prec >= 0 && len(digits) < prec {
		digits = strings.Repeat("0", prec-len(digits)) + digits
	}
	total := len(sign) + len(prefix) + len(digits)
	pad := 0
	if width > total {
		pad = width - total
	}
	if flags&flagMinus != 0 {
		out = append(out, sign...)
		out = append(out, prefix...)
		out = append(out, digits...)
		for i := 0; i < pad; i++ {
			out = append(out, ' ')
		}
		return out
	}
	if flags&flagZero != 0 && prec < 0 {
		out = append(out, sign...)
		out = append(out, prefix...)
		for i := 0; i < pad; i++ {
			out = append(out, '0')
		}
		out = append(out, digits...)
		return out
	}
	for i := 0; i < pad; i++ {
		out = append(out, ' ')
	}
	out = append(out, sign...)
	out = append(out, prefix...)
	out = append(out, digits...)
	return out
}

func copyToGuest(dst uintptr, b []byte) {
	for i := 0; i < len(b); i++ {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = b[i]
	}
}

// --- the stdio entry points reached during startup -----------------------

// goVsnprintf implements vsnprintf(char *str, size_t size, const char *fmt,
// va_list ap): it returns the length the full output would have had, truncating
// to size-1 bytes plus a NUL when size > 0, exactly as C requires.
//
//go:noinline
//go:nosplit
func goVsnprintf(str, size, format, ap uintptr) int {
	if format == 0 {
		if str != 0 && size != 0 {
			*(*byte)(unsafe.Pointer(str)) = 0
		}
		return 0
	}
	// A null destination with size 0 is the standard "how long would it be?"
	// probe that C++ formatting helpers make before allocating; it must
	// return the real formatted length. (An earlier boot-time shortcut that
	// returned an empty string, or the format's own length, left every
	// engine-formatted string empty - log file names, paths, URLs.)
	out := formatC(cstr(format), (*vaList)(unsafe.Pointer(ap)))
	n := len(out)
	if size != 0 {
		limit := int64(size) - 1
		c := int64(n)
		if c > limit {
			c = limit
		}
		if c > 0 {
			copyToGuest(str, out[:c])
		}
		*(*byte)(unsafe.Pointer(str + uintptr(c))) = 0
	}
	return n
}

// goVsnprintfChk is the fortify variant __vsnprintf_chk(dst, len, flag, buflen,
// fmt, ap). The extra arguments only describe the destination, so they are
// ignored beyond using the same bounded copy as goVsnprintf.
//
//go:noinline
//go:nosplit
func goVsnprintfChk(a1, a2, a3, a4, a5, a6 uintptr) int {
	_ = a3
	_ = a4
	return goVsnprintf(a1, a2, a5, a6)
}

// goVsprintf implements vsprintf(char *str, const char *fmt, va_list ap).
//
//go:noinline
func goVsprintf(str, format, ap uintptr) int {
	out := formatC(cstr(format), (*vaList)(unsafe.Pointer(ap)))
	copyToGuest(str, out)
	*(*byte)(unsafe.Pointer(str + uintptr(len(out)))) = 0
	return len(out)
}
