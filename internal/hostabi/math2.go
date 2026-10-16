package hostabi

import (
	"math"
	"strconv"
	"unsafe"
)

// Additional math entry points the engine imports. Float arguments and results
// travel in XMM registers identically under the SysV and Go ABIs, so these are
// ordinary Go functions (see math.go). The long-double entry points (powl,
// fmal, strtold_l) are deliberately absent: x86-64 long double is 80-bit x87,
// which has no Go equivalent, so binding them would silently return garbage.

//go:noinline
func goAcosF(x float32) float32 { return float32(math.Acos(float64(x))) }

//go:noinline
func goAsinF(x float32) float32 { return float32(math.Asin(float64(x))) }

//go:noinline
func goAtanF(x float32) float32 { return float32(math.Atan(float64(x))) }

//go:noinline
func goAtan2F(y, x float32) float32 { return float32(math.Atan2(float64(y), float64(x))) }

//go:noinline
func goCbrtF(x float32) float32 { return float32(math.Cbrt(float64(x))) }

//go:noinline
func goCoshF(x float32) float32 { return float32(math.Cosh(float64(x))) }

//go:noinline
func goSinhF(x float32) float32 { return float32(math.Sinh(float64(x))) }

//go:noinline
func goTanhF(x float32) float32 { return float32(math.Tanh(float64(x))) }

//go:noinline
func goErfF(x float32) float32 { return float32(math.Erf(float64(x))) }

//go:noinline
func goErfcF(x float32) float32 { return float32(math.Erfc(float64(x))) }

//go:noinline
func goExp2F(x float32) float32 { return float32(math.Exp2(float64(x))) }

//go:noinline
func goLog2F(x float32) float32 { return float32(math.Log2(float64(x))) }

//go:noinline
func goNextafterF(x, y float32) float32 { return float32(math.Nextafter(float64(x), float64(y))) }

//go:noinline
func goRemainderF(x, y float32) float32 { return float32(math.Remainder(float64(x), float64(y))) }

//go:noinline
func goRemquoF(x, y float32, quo uintptr) float32 {
	q := math.Remainder(float64(x), float64(y))
	// The quotient is only loosely specified; report the nearest integer.
	*(*int32)(unsafe.Pointer(quo)) = int32(math.RoundToEven((float64(x) - q) / float64(y)))
	return float32(q)
}

//go:noinline
func goModfF(x float32, iptr uintptr) float32 {
	ip := math.Trunc(float64(x))
	*(*float32)(unsafe.Pointer(iptr)) = float32(ip)
	return float32(float64(x) - ip)
}

//go:noinline
func goFrexpF(x float32, exp uintptr) float32 {
	f, e := math.Frexp(float64(x))
	*(*int32)(unsafe.Pointer(exp)) = int32(e)
	return float32(f)
}

//go:noinline
func goLdexpf(x float32, e uintptr) float32 { return float32(math.Ldexp(float64(x), int(int32(e)))) }

//go:noinline
func goFinitef(x float32) int {
	if math.IsInf(float64(x), 0) || math.IsNaN(float64(x)) {
		return 0
	}
	return 1
}

//go:noinline
func goIlogb(x float64) int { return math.Ilogb(x) }

//go:noinline
func goLlroundf(x float32) int64 { return int64(math.RoundToEven(float64(x))) }

//go:noinline
func goLroundf(x float32) int64 { return int64(math.RoundToEven(float64(x))) }

//go:noinline
func goLlround(x float64) int64 { return int64(math.RoundToEven(x)) }

//go:noinline
func goLround(x float64) int64 { return int64(math.RoundToEven(x)) }

//go:noinline
func goSincos(x float64, sinp, cosp uintptr) {
	s, c := math.Sincos(x)
	*(*float64)(unsafe.Pointer(sinp)) = s
	*(*float64)(unsafe.Pointer(cosp)) = c
}

//go:noinline
func goSincosF(x float32, sinp, cosp uintptr) {
	s, c := math.Sincos(float64(x))
	*(*float32)(unsafe.Pointer(sinp)) = float32(s)
	*(*float32)(unsafe.Pointer(cosp)) = float32(c)
}

// --- string to float -----------------------------------------------------

var floatChars = func() [256]bool {
	var t [256]bool
	for _, c := range []byte("0123456789abcdefABCDEFxXpP+-.eE") {
		t[c] = true
	}
	return t
}()

// scanFloat finds the longest prefix of the C string at p that Go can parse as
// a float and returns the value and the number of bytes consumed. C accepts a
// superset (hex floats, inf/nan); Go's parser handles those too, so repeatedly
// shrinking the candidate is both simple and correct.
func scanFloat(p uintptr) (float64, uintptr, bool) {
	i := uintptr(0)
	for {
		c := *(*byte)(unsafe.Pointer(p + i))
		if !floatChars[c] {
			break
		}
		i++
	}
	for ; i > 0; i-- {
		buf := make([]byte, i)
		for j := uintptr(0); j < i; j++ {
			buf[j] = *(*byte)(unsafe.Pointer(p + j))
		}
		if v, err := strconv.ParseFloat(string(buf), 64); err == nil {
			return v, i, true
		}
	}
	return 0, 0, false
}

//go:noinline
func goStrtod(nptr, endp uintptr) float64 {
	s := nptr
	for isSpace(*(*byte)(unsafe.Pointer(s))) {
		s++
	}
	v, n, ok := scanFloat(s)
	if !ok {
		if endp != 0 {
			*(*uintptr)(unsafe.Pointer(endp)) = nptr
		}
		return 0
	}
	if endp != 0 {
		*(*uintptr)(unsafe.Pointer(endp)) = s + n
	}
	return v
}

//go:noinline
func goStrtodL(nptr, endp, loc uintptr) float64 { return goStrtod(nptr, endp) }

//go:noinline
func goStrtof(nptr, endp uintptr) float32 { return float32(goStrtod(nptr, endp)) }

//go:noinline
func goStrtofL(nptr, endp, loc uintptr) float32 { return float32(goStrtod(nptr, endp)) }

//go:noinline
func goAtof(s uintptr) float64 { return goStrtod(s, 0) }

//go:noinline
func goNan(tag uintptr) float64 { return math.NaN() }

// nonNullCStr is used where a C string is required but the input may be NULL.
func nonNullCStr(p uintptr) uintptr {
	if p == 0 {
		return cstringConst("")
	}
	return p
}
