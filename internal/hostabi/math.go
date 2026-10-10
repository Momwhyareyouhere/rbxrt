package hostabi

import "math"

// Pure floating-point functions need no register translation on amd64: the
// SysV and Go ABIs both take float arguments in XMM0.. and return in XMM0. The
// thunk still calls them as ordinary Go functions.

//go:noinline
func goSin(x float64) float64 { return math.Sin(x) }

//go:noinline
func goCos(x float64) float64 { return math.Cos(x) }

//go:noinline
func goTan(x float64) float64 { return math.Tan(x) }

//go:noinline
func goAsin(x float64) float64 { return math.Asin(x) }

//go:noinline
func goAcos(x float64) float64 { return math.Acos(x) }

//go:noinline
func goAtan(x float64) float64 { return math.Atan(x) }

//go:noinline
func goAtan2(y, x float64) float64 { return math.Atan2(y, x) }

//go:noinline
func goSinh(x float64) float64 { return math.Sinh(x) }

//go:noinline
func goCosh(x float64) float64 { return math.Cosh(x) }

//go:noinline
func goTanh(x float64) float64 { return math.Tanh(x) }

//go:noinline
func goExp(x float64) float64 { return math.Exp(x) }

//go:noinline
func goExp2(x float64) float64 { return math.Exp2(x) }

//go:noinline
func goExpm1(x float64) float64 { return math.Expm1(x) }

//go:noinline
func goLog(x float64) float64 { return math.Log(x) }

//go:noinline
func goLog2(x float64) float64 { return math.Log2(x) }

//go:noinline
func goLog10(x float64) float64 { return math.Log10(x) }

//go:noinline
func goLog1p(x float64) float64 { return math.Log1p(x) }

//go:noinline
func goCbrt(x float64) float64 { return math.Cbrt(x) }

//go:noinline
func goSqrt(x float64) float64 { return math.Sqrt(x) }

//go:noinline
func goPow(x, y float64) float64 { return math.Pow(x, y) }

//go:noinline
func goHypot(x, y float64) float64 { return math.Hypot(x, y) }

//go:noinline
func goFabs(x float64) float64 { return math.Abs(x) }

//go:noinline
func goCeil(x float64) float64 { return math.Ceil(x) }

//go:noinline
func goFloor(x float64) float64 { return math.Floor(x) }

//go:noinline
func goRound(x float64) float64 { return math.Round(x) }

//go:noinline
func goTrunc(x float64) float64 { return math.Trunc(x) }

//go:noinline
func goFmod(x, y float64) float64 { return math.Mod(x, y) }

//go:noinline
func goRemainder(x, y float64) float64 { return math.Remainder(x, y) }

//go:noinline
func goCopysign(x, y float64) float64 { return math.Copysign(x, y) }

//go:noinline
func goFma(x, y, z float64) float64 { return math.FMA(x, y, z) }

//go:noinline
func goErf(x float64) float64 { return math.Erf(x) }

//go:noinline
func goErfc(x float64) float64 { return math.Erfc(x) }

// Single-precision variants: the controller passes a float32 in XMM0 and
// expects a float32 back, so these adapt around the float64 implementations.
//
//go:noinline
func goSinF(x float32) float32 { return float32(math.Sin(float64(x))) }

//go:noinline
func goCosF(x float32) float32 { return float32(math.Cos(float64(x))) }

//go:noinline
func goTanF(x float32) float32 { return float32(math.Tan(float64(x))) }

//go:noinline
func goExpF(x float32) float32 { return float32(math.Exp(float64(x))) }

//go:noinline
func goLogF(x float32) float32 { return float32(math.Log(float64(x))) }

//go:noinline
func goLog10F(x float32) float32 { return float32(math.Log10(float64(x))) }

//go:noinline
func goPowF(x, y float32) float32 { return float32(math.Pow(float64(x), float64(y))) }

//go:noinline
func goFmodF(x, y float32) float32 { return float32(math.Mod(float64(x), float64(y))) }

//go:noinline
func goSqrtF(x float32) float32 { return float32(math.Sqrt(float64(x))) }

//go:noinline
func goFloorF(x float32) float32 { return float32(math.Floor(float64(x))) }

//go:noinline
func goCeilF(x float32) float32 { return float32(math.Ceil(float64(x))) }

//go:noinline
func goRoundF(x float32) float32 { return float32(math.Round(float64(x))) }

//go:noinline
func goHypotF(x, y float32) float32 { return float32(math.Hypot(float64(x), float64(y))) }
