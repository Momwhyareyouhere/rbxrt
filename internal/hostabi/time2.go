package hostabi

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// struct tm (bionic x86-64): nine ints, then a long tm_gmtoff and a char*
// tm_zone. Offsets: sec=0 min=4 hour=8 mday=12 mon=16 year=20 wday=24 yday=28
// isdst=32, gmtoff=40, zone=48; total size 56.
const tmSize = 56

var (
	zoneMu      sync.Mutex
	zoneStrings = map[string]uintptr{}
)

// zoneString interns a time-zone name so repeated localtime calls do not keep
// allocating new C strings.
func zoneString(name string) uintptr {
	zoneMu.Lock()
	defer zoneMu.Unlock()
	if p, ok := zoneStrings[name]; ok {
		return p
	}
	p := cstringConst(name)
	zoneStrings[name] = p
	return p
}

func writeTM(dst uintptr, t time.Time, local bool) {
	_, off := t.Zone()
	zone := t.Location().String()
	put32 := func(o uintptr, v int) { *(*int32)(unsafe.Pointer(dst + o)) = int32(v) }
	put32(0, t.Second())
	put32(4, t.Minute())
	put32(8, t.Hour())
	put32(12, t.Day())
	put32(16, int(t.Month())-1)
	put32(20, t.Year()-1900)
	put32(24, int(t.Weekday()))
	put32(28, t.YearDay()-1)
	put32(32, 0) // tm_isdst
	*(*int64)(unsafe.Pointer(dst + 40)) = int64(off)
	*(*uintptr)(unsafe.Pointer(dst + 48)) = zoneString(zone)
}

//go:noinline
func goGmtimeR(tp, result uintptr) uintptr {
	if tp == 0 || result == 0 {
		return 0
	}
	sec := *(*int64)(unsafe.Pointer(tp))
	writeTM(result, time.Unix(sec, 0).UTC(), false)
	return result
}

//go:noinline
func goLocaltimeR(tp, result uintptr) uintptr {
	if tp == 0 || result == 0 {
		return 0
	}
	sec := *(*int64)(unsafe.Pointer(tp))
	writeTM(result, time.Unix(sec, 0).Local(), true)
	return result
}

var staticTM [tmSize]byte

//go:noinline
func goGmtime(tp uintptr) uintptr {
	if tp == 0 {
		return 0
	}
	return goGmtimeR(tp, uintptr(unsafe.Pointer(&staticTM[0])))
}

//go:noinline
func goLocaltime(tp uintptr) uintptr {
	if tp == 0 {
		return 0
	}
	return goLocaltimeR(tp, uintptr(unsafe.Pointer(&staticTM[0])))
}

//go:noinline
func goMktime(tm uintptr) int64 {
	get32 := func(o uintptr) int { return int(*(*int32)(unsafe.Pointer(tm + o))) }
	year := get32(20) + 1900
	month := get32(16) + 1
	day := get32(12)
	hour := get32(8)
	min := get32(4)
	sec := get32(0)
	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	writeTM(tm, t, true)
	return t.Unix()
}

//go:noinline
func goDifftime(t1, t2 uintptr) float64 { return float64(int64(t1) - int64(t2)) }

//go:noinline
func goTzset() {}

// --- strftime -------------------------------------------------------------

// C locale names (English), which is both what bionic's default locale prints
// and what Go's time package abbreviations spell.
var (
	strftimeWeekShort  = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	strftimeWeekLong   = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	strftimeMonthShort = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	strftimeMonthLong  = [12]string{"January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December"}
)

//go:noinline
func goStrftime(buf, max, format, tmAddr uintptr) uintptr {
	return strftimeInto(buf, max, format, tmAddr)
}

//go:noinline
func goStrftimeL(buf, max, format, tmAddr, loc uintptr) uintptr {
	// The runtime has one locale: the C locale's English names, which are also
	// the only ones Go's time package produces. bionic's locale_t only adds
	// number formatting differences that do not apply to strftime.
	_ = loc
	return strftimeInto(buf, max, format, tmAddr)
}

// strftimeInto formats into the guest buffer under C's rules: the return value
// is the byte count written, excluding the terminator, and 0 (with the buffer
// left alone) when the result, terminator included, would not fit in max.
func strftimeInto(buf, max, format, tmAddr uintptr) uintptr {
	if buf == 0 || max == 0 || format == 0 || tmAddr == 0 {
		return 0
	}
	out := formatTM(cstr(format), tmAddr)
	if uintptr(len(out))+1 > max {
		return 0
	}
	for i := 0; i < len(out); i++ {
		*(*byte)(unsafe.Pointer(buf + uintptr(i))) = out[i]
	}
	*(*byte)(unsafe.Pointer(buf + uintptr(len(out)))) = 0
	return uintptr(len(out))
}

// formatTM renders a strftime format string against the guest's struct tm.
func formatTM(format string, tmAddr uintptr) string {
	get := func(off uintptr) int { return int(*(*int32)(unsafe.Pointer(tmAddr + off))) }
	sec, min, hour := get(0), get(4), get(8)
	mday, mon0, year0 := get(12), get(16), get(20)
	wday, yday := get(24), get(28)
	gmtoff := int(*(*int64)(unsafe.Pointer(tmAddr + 40)))
	zone := ""
	if p := *(*uintptr)(unsafe.Pointer(tmAddr + 48)); p != 0 {
		zone = cstr(p)
	}
	// Defensive clamps: the fields are read straight from guest memory and an
	// index panic would say nothing about the engine's behaviour.
	if mon0 < 0 {
		mon0 = 0
	} else if mon0 > 11 {
		mon0 = 11
	}
	if wday < 0 || wday > 6 {
		wday = 0
	}
	year := year0 + 1900
	t := time.Date(year, time.Month(mon0+1), mday, hour, min, sec, 0, time.Local)
	isoYear, isoWeek := t.ISOWeek()
	hour12 := (hour+11)%12 + 1
	meridiem := "AM"
	if hour >= 12 {
		meridiem = "PM"
	}
	// Week numbers per POSIX: %U counts from the Sunday on or before Jan 1,
	// %W from the Monday on or before it.
	weekU := (yday + 7 - wday) / 7
	weekW := (yday + 7 - (wday+6)%7) / 7

	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) {
			break // dangling '%': POSIX leaves it undefined; emit nothing
		}
		// Flags and modifiers: 0/-/_ choose padding, O/E choose an alternative
		// representation the runtime does not have, so both are ignored.
		padflag := byte(0)
		width := 0
		for i < len(format) {
			c := format[i]
			if c == '0' || c == '-' || c == '_' {
				padflag = c
				i++
				continue
			}
			if c >= '1' && c <= '9' {
				ds := i
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					i++
				}
				width, _ = strconv.Atoi(format[ds:i])
				continue
			}
			if c == 'E' || c == 'O' {
				i++
				continue
			}
			break
		}
		if i >= len(format) {
			break
		}
		spec := format[i]
		// num formats a numeric conversion: default width and default padding
		// come from the conversion itself, overridden by flags and by an
		// explicit width.
		num := func(v, defWidth int, defPad byte) string {
			s := strconv.Itoa(v)
			w := defWidth
			if width > 0 {
				w = width
			}
			p := defPad
			switch padflag {
			case '-':
				p = 0
			case '_':
				p = ' '
			case '0':
				p = '0'
			}
			for p != 0 && len(s) < w {
				s = string(p) + s
			}
			return s
		}
		switch spec {
		case 'a':
			b.WriteString(strftimeWeekShort[wday])
		case 'A':
			b.WriteString(strftimeWeekLong[wday])
		case 'b', 'h':
			b.WriteString(strftimeMonthShort[mon0])
		case 'B':
			b.WriteString(strftimeMonthLong[mon0])
		case 'c':
			// C locale default representation.
			fmt.Fprintf(&b, "%s %s %s %02d:%02d:%02d %04d",
				strftimeWeekShort[wday], strftimeMonthShort[mon0],
				strftimeDay(mday, padflag, width), hour, min, sec, year)
		case 'C':
			b.WriteString(num(year/100, 2, '0'))
		case 'd':
			b.WriteString(num(mday, 2, '0'))
		case 'D':
			fmt.Fprintf(&b, "%02d/%02d/%02d", mon0+1, mday, year0%100)
		case 'e':
			b.WriteString(num(mday, 2, ' '))
		case 'F':
			fmt.Fprintf(&b, "%04d-%02d-%02d", year, mon0+1, mday)
		case 'g':
			b.WriteString(num(isoYear%100, 2, '0'))
		case 'G':
			b.WriteString(num(isoYear, 4, '0'))
		case 'H':
			b.WriteString(num(hour, 2, '0'))
		case 'I':
			b.WriteString(num(hour12, 2, '0'))
		case 'j':
			b.WriteString(num(yday+1, 3, '0'))
		case 'k':
			b.WriteString(num(hour, 2, ' '))
		case 'l':
			b.WriteString(num(hour12, 2, ' '))
		case 'm':
			b.WriteString(num(mon0+1, 2, '0'))
		case 'M':
			b.WriteString(num(min, 2, '0'))
		case 'n':
			b.WriteByte('\n')
		case 'p':
			b.WriteString(meridiem)
		case 'P':
			b.WriteString(strings.ToLower(meridiem))
		case 'r':
			fmt.Fprintf(&b, "%02d:%02d:%02d %s", hour12, min, sec, meridiem)
		case 'R':
			fmt.Fprintf(&b, "%02d:%02d", hour, min)
		case 'S':
			b.WriteString(num(sec, 2, '0'))
		case 's':
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 't':
			b.WriteByte('\t')
		case 'T':
			fmt.Fprintf(&b, "%02d:%02d:%02d", hour, min, sec)
		case 'u':
			wd := wday
			if wd == 0 {
				wd = 7
			}
			b.WriteString(num(wd, 1, '0'))
		case 'U':
			b.WriteString(num(weekU, 2, '0'))
		case 'V':
			b.WriteString(num(isoWeek, 2, '0'))
		case 'w':
			b.WriteString(num(wday, 1, '0'))
		case 'W':
			b.WriteString(num(weekW, 2, '0'))
		case 'x':
			fmt.Fprintf(&b, "%02d/%02d/%02d", mon0+1, mday, year0%100)
		case 'X':
			fmt.Fprintf(&b, "%02d:%02d:%02d", hour, min, sec)
		case 'y':
			b.WriteString(num(year0%100, 2, '0'))
		case 'Y':
			b.WriteString(num(year, 4, '0'))
		case 'z':
			off := gmtoff
			sign := byte('+')
			if off < 0 {
				sign, off = '-', -off
			}
			fmt.Fprintf(&b, "%c%02d%02d", sign, off/3600, (off%3600)/60)
		case 'Z':
			b.WriteString(zone)
		case '%':
			b.WriteByte('%')
		default:
			// Unsupported conversion: reproduce it so a caller relying on one
			// shows up in the output instead of being silently dropped.
			b.WriteByte('%')
			b.WriteByte(spec)
		}
	}
	return b.String()
}

// strftimeDay renders a day of month with the padding of a spec that defaults
// to spaces, honouring the caller's flags (%c embeds %e).
func strftimeDay(mday int, padflag byte, width int) string {
	w := 2
	if width > 0 {
		w = width
	}
	p := byte(' ')
	switch padflag {
	case '-':
		p = 0
	case '_':
		p = ' '
	case '0':
		p = '0'
	}
	s := strconv.Itoa(mday)
	for p != 0 && len(s) < w {
		s = string(p) + s
	}
	return s
}

//go:noinline
func goClock() int64 {
	// CLOCKS_PER_SEC is 1000000 on bionic; report elapsed process time.
	return monotonicNowNanos() / 1000
}
