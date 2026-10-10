package loader

import (
	"rbxrt/internal/hostabi"
)

// RunInit invokes the DSO's constructors in order: DT_INIT first, then each
// entry of DT_INIT_ARRAY, as bionic does. Guest code runs on the current
// goroutine's stack.
//
// Caveat (documented, not hidden): the garbage collector cannot scan guest
// frames on the Go stack. We mitigate by minimizing the Go work between guest
// calls, but the correct long-term model is a dedicated OS thread with a mmap'd
// guest stack, which is outside this milestone. If guest code grows the stack
// and triggers a Go stack copy, the runtime may misbehave.
func (im *Image) RunInit() error {
	for _, fn := range im.InitFuncs {
		hostabi.CallGuestVoid(fn)
	}
	return nil
}

// RunFini invokes the DSO's destructors. Intended for process teardown only.
func (im *Image) RunFini() {
	for _, fn := range im.FiniFuncs {
		hostabi.CallGuestVoid(fn)
	}
}

// Call0 runs a guest function taking no arguments and returning no result. It
// is exposed so callers can start guest code (e.g. JNI_OnLoad or a library
// entry point) in the same controlled way RunInit does.
func (im *Image) Call0(fn uintptr) {
	hostabi.CallGuestVoid(fn)
}
