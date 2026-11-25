package hostabi

import (
	"testing"
	"unsafe"
)

// newVaList builds a SysV va_list in pinned memory whose integer register save
// area holds args (at most six).
func newVaList(t *testing.T, args ...uintptr) uintptr {
	t.Helper()
	mem, _ := pin(t) // released with the test process; tiny
	save := mem + 64
	for i, a := range args {
		*(*uintptr)(unsafe.Pointer(save + uintptr(i)*8)) = a
	}
	va := (*vaList)(unsafe.Pointer(mem))
	*va = vaList{gpOffset: 0, fpOffset: 48, overflow: mem + 2048, regSave: save}
	return mem
}

// C++ formatting helpers size their buffer with vsnprintf(NULL, 0, ...) and
// then format into it; both calls must agree on the real length.
func TestVsnprintfSizeProbeAndFormat(t *testing.T) {
	format, unmapF := putCString(t, "%s_%s.log")
	defer unmapF()
	a, unmapA := putCString(t, "player")
	defer unmapA()
	b, unmapB := putCString(t, "1793115657")
	defer unmapB()

	if n := goVsnprintf(0, 0, format, newVaList(t, a, b)); n != len("player_1793115657.log") {
		t.Fatalf("size probe = %d, want %d", n, len("player_1793115657.log"))
	}
	buf, unmapBuf := pin(t)
	defer unmapBuf()
	n := goVsnprintf(buf, 64, format, newVaList(t, a, b))
	if got := cstrAt(buf, 64); got != "player_1793115657.log" || n != len(got) {
		t.Fatalf("vsnprintf = %q (n=%d)", got, n)
	}
	// Truncation keeps the NUL and still reports the full length.
	n = goVsnprintf(buf, 7, format, newVaList(t, a, b))
	if got := cstrAt(buf, 64); got != "player" || n != len("player_1793115657.log") {
		t.Fatalf("truncated vsnprintf = %q (n=%d)", got, n)
	}
}
