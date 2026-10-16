package hostabi

import "runtime/debug"

// init disables the Go garbage collector. Guest frames currently live on the Go
// goroutine stack (the dedicated mmap'd guest stack has not landed yet), so a
// GC stack scan that walks through a host->guest transition throws "unknown
// caller pc". The host's own allocations are modest next to the guest's
// (tcmalloc-tracking), so letting the host heap grow is acceptable for now; the
// guest-stack redesign is what lifts this limitation.
func init() {
	debug.SetGCPercent(-1)
}
