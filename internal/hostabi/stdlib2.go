package hostabi

import "unsafe"

// --- pseudo-random numbers ----------------------------------------------
//
// A small xorshift generator with the C rand()/srand() interface. The engine
// only uses these for non-cryptographic purposes; cryptographic randomness is
// provided separately (arc4random).

var randState uint64 = 0x2545F4914F6CDD1D

//go:noinline
func goSrand(seed uintptr) {
	randState = uint64(uint32(seed))*6364136223846793005 + 1442695040888963407
	if randState == 0 {
		randState = 1
	}
}

//go:noinline
func goRand() int {
	x := randState
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	randState = x
	return int(x >> 33) // 0 .. RAND_MAX (2^31-1)
}

// --- sorting and searching -----------------------------------------------

func elemAt(base uintptr, i, size uintptr) uintptr { return base + i*size }

//go:noinline
func goQsort(base, nmemb, size, compar uintptr) {
	if nmemb < 2 || size == 0 {
		return
	}
	// Insertion sort: simple, stable enough for the ordering the guest only
	// needs, and it invokes the guest comparator exactly as libc does.
	var tmp [64]byte
	useTmp := size <= uintptr(len(tmp))
	for i := uintptr(1); i < nmemb; i++ {
		a := elemAt(base, i, size)
		var j uintptr = i
		for j > 0 {
			b := elemAt(base, j-1, size)
			if callGuestCInt(compar, a, b, 0) >= 0 {
				break
			}
			// swap a and b
			if useTmp {
				goMemcpy(uintptr(unsafe.Pointer(&tmp[0])), a, size)
				goMemcpy(a, b, size)
				goMemcpy(b, uintptr(unsafe.Pointer(&tmp[0])), size)
			} else {
				swapBytes(a, b, size)
			}
			a = b
			j--
		}
	}
}

func swapBytes(a, b, n uintptr) {
	for i := uintptr(0); i < n; i++ {
		x := *(*byte)(unsafe.Pointer(a + i))
		*(*byte)(unsafe.Pointer(a + i)) = *(*byte)(unsafe.Pointer(b + i))
		*(*byte)(unsafe.Pointer(b + i)) = x
	}
}

//go:noinline
func goBsearch(key, base, nmemb, size, compar uintptr) uintptr {
	lo, hi := uintptr(0), nmemb
	for lo < hi {
		mid := lo + (hi-lo)/2
		p := elemAt(base, mid, size)
		c := callGuestCInt(compar, key, p, 0)
		switch {
		case c < 0:
			hi = mid
		case c > 0:
			lo = mid + 1
		default:
			return p
		}
	}
	return 0
}

//go:noinline
func goAbs(x uintptr) int {
	v := int(x)
	if v < 0 {
		return -v
	}
	return v
}

//go:noinline
func goLabs(x uintptr) int64 { return int64(goAbs(x)) }

//go:noinline
func goLabs2(x uintptr) int64 { return int64(goAbs(x)) }
