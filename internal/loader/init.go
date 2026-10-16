package loader

import (
	"fmt"
	"os"

	"rbxrt/internal/hostabi"
)

// RunInit invokes the DSO's constructors in order: DT_INIT first, then each
// entry of DT_INIT_ARRAY, as bionic does. Guest code runs on the current
// goroutine's stack via the host ABI trampolines.
//
// Caveat (documented, not hidden): the garbage collector cannot scan guest
// frames on the Go stack, so the GC is inhibited globally while the guest runs
// (see hostabi's init). The host's own allocation during startup is small
// (measured in the low tens of MB), so letting the heap grow uncollected is
// acceptable here; the durable fix is a dedicated OS thread with an mmap'd
// guest stack, which is outside this milestone.
//
// When Trace is set, each constructor is reported before it runs so that a
// fault (e.g. a stack-smashing abort) can be attributed to a specific
// initializer.
func (im *Image) RunInit() error {
	hostabi.SetGuestBias(im.Bias)
	for i, fn := range im.InitFuncs {
		if im.Trace {
			fmt.Fprintf(os.Stderr, "rbxrt: init[%d/%d] vaddr=0x%x\n",
				i, len(im.InitFuncs), fn-im.Bias)
		}
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
