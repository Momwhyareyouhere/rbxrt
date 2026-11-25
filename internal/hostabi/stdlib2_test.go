package hostabi

import (
	"testing"
	"unsafe"
)

// cmpUint32Stub is a comparator compiled the way OpenSSL's cipher-id compare
// is: `movl (%rdi),%eax; subl (%rsi),%eax; ret`. A negative result lives only
// in %eax; the upper half of %rax is zero.
var cmpUint32Stub = []byte{0x8b, 0x07, 0x2b, 0x06, 0xc3}

func TestQsortBsearchGuestComparator(t *testing.T) {
	stub := codeArena(len(cmpUint32Stub))
	copy(stub, cmpUint32Stub)
	arenaMakeExec()
	compar := uintptr(unsafe.Pointer(&stub[0]))

	// 12-byte records keyed by their first uint32, like a small cipher table.
	mem, unmap := pin(t)
	defer unmap()
	ids := []uint32{0x03001302, 0x0300C02C, 0x03001301, 0x0300009F, 0x03001303, 0x0300002F}
	for i, id := range ids {
		*(*uint32)(unsafe.Pointer(mem + uintptr(i)*12)) = id
	}
	goQsort(mem, uintptr(len(ids)), 12, compar)
	for i := 1; i < len(ids); i++ {
		a := *(*uint32)(unsafe.Pointer(mem + uintptr(i-1)*12))
		b := *(*uint32)(unsafe.Pointer(mem + uintptr(i)*12))
		if a > b {
			t.Fatalf("qsort left %#x before %#x", a, b)
		}
	}
	key := mem + 2048
	for _, id := range ids {
		*(*uint32)(unsafe.Pointer(key)) = id
		p := goBsearch(key, mem, uintptr(len(ids)), 12, compar)
		if p == 0 || *(*uint32)(unsafe.Pointer(p)) != id {
			t.Fatalf("bsearch did not find %#x", id)
		}
	}
	*(*uint32)(unsafe.Pointer(key)) = 0x03001304
	if p := goBsearch(key, mem, uintptr(len(ids)), 12, compar); p != 0 {
		t.Fatalf("bsearch found an absent key at %#x", p)
	}
}
