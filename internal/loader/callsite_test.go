package loader

import (
	"testing"

	"rbxrt/internal/engine"
)

// TestCallSiteName pins the decoding the guest-thread sampler relies on: at
// vaddr 0x2dde8a2 the engine does `call syscall@plt`, so the address of the
// next instruction must resolve to the import name "syscall". Anything less
// would leave the sampler reporting bare addresses.
func TestCallSiteName(t *testing.T) {
	path := genuineLib()
	if path == "" {
		t.Skip("no genuine libroblox.so available (set RBXRT_GENUINE)")
	}
	b, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	b.ClassifyImports()
	im, err := Load(b, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	const site = 0x2dde8a7 // return address of `call syscall@plt` at 0x2dde8a2
	if got, want := im.CallSiteName(im.Rebase(site)), "syscall"; got != want {
		t.Errorf("CallSiteName(image+0x%x) = %q, want %q", site, got, want)
	}

	// Addresses that are not calls, and addresses outside the image, must
	// decline rather than guess at a name.
	if got := im.CallSiteName(im.Rebase(0)); got != "" {
		t.Errorf("CallSiteName(image start) = %q, want empty", got)
	}
	if got := im.CallSiteName(im.Base - 8); got != "" {
		t.Errorf("CallSiteName(below image) = %q, want empty", got)
	}
	if got := im.CallSiteName(im.End); got != "" {
		t.Errorf("CallSiteName(at image end) = %q, want empty", got)
	}
}
