package hostabi

import (
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

//go:noinline
func goClock() int64 {
	// CLOCKS_PER_SEC is 1000000 on bionic; report elapsed process time.
	return monotonicNowNanos() / 1000
}
