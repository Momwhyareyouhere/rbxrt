package hostabi

import (
	"testing"
	"unsafe"
)

// TestFS28Stability checks whether the word at FS:+0x28 (often used by the
// guest as a stack canary) is stable across calls to host functions. If the
// Go runtime uses that word for something that changes (e.g. a stackguard),
// the test will fail, indicating that a guest TCB is needed for correct
// canary handling.
func TestFS28Stability(t *testing.T) {
	before := readFS28()
	// Call a variety of host functions that may trigger Go stack growth or
	// TLS updates. These are representative of what constructors invoke.
	for i := 0; i < 1000; i++ {
		// Use small buffers to avoid huge allocations.
		var buf [8]byte
		goMemcpy(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&buf[0])), 8)
		goMemset(uintptr(unsafe.Pointer(&buf[0])), 0, 8)
		goStrlen(uintptr(unsafe.Pointer(&buf[0])))
		goStrcmp(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&buf[0])))
		goStrcpy(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&buf[0])))
		goAtoi(uintptr(unsafe.Pointer(&buf[0])))
	}
	after := readFS28()
	if before != after {
		t.Fatalf("FS+0x28 changed during host calls: %#x → %#x", before, after)
	}
}
