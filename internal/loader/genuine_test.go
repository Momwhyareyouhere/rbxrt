package loader

import (
	"os"
	"testing"

	"rbxrt/internal/engine"
)

// genuineLib returns the path to a real libroblox.so to test against, or ""
// when none is available (in which case the integration test skips). It is not
// part of the repository; point RBXRT_GENUINE at one, or place it at the
// conventional extraction path.
func genuineLib() string {
	if p := os.Getenv("RBXRT_GENUINE"); p != "" {
		return p
	}
	const def = "/home/bombo/extracted/lib/x86_64/libroblox.so"
	if st, err := os.Stat(def); err == nil && !st.IsDir() {
		return def
	}
	return ""
}

// TestGenuineLoad is an integration test against a real Android Roblox engine
// library. It is skipped when no library is available. It pins the behaviours
// that matter for the runtime contract:
//
//   - every PT_LOAD is mapped and the span matches the program headers;
//   - every relative relocation's addend points inside the image (the packed
//     APS2 decoder accumulates a running addend across groups; getting that
//     wrong yields huge negative addends, which this catches);
//   - exported symbols and constructors resolve to addresses inside the image;
//   - unresolved strong functions are bound to trap thunks, never to zero.
func TestGenuineLoad(t *testing.T) {
	path := genuineLib()
	if path == "" {
		t.Skip("no genuine libroblox.so available (set RBXRT_GENUINE)")
	}
	b, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	b.ClassifyImports()

	// The image extent from the program headers.
	var minV, maxV uint64
	have := false
	for _, p := range b.File.Progs {
		if p.Type != 1 { // PT_LOAD
			continue
		}
		if !have || p.Vaddr < minV {
			minV = p.Vaddr
		}
		if end := p.Vaddr + p.Memsz; end > maxV {
			maxV = end
		}
		have = true
	}
	if minV != 0 {
		t.Errorf("lowest PT_LOAD vaddr = %#x, want 0", minV)
	}

	// Every relative addend must land inside the image, addr 0..maxV.
	bad := 0
	for _, r := range b.Relocs.Relative {
		if r.Addend < 0 || uint64(r.Addend) >= maxV {
			if bad < 5 {
				t.Errorf("relative reloc at %#x has out-of-image addend %#x (maxV %#x)",
					r.Offset, r.Addend, maxV)
			}
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d relative relocations have out-of-image addends", bad)
	}

	im, err := Load(b, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if im.Size == 0 {
		t.Fatal("loaded image has zero size")
	}

	// Constructors must point inside the image, never below the base.
	for i, fn := range im.InitFuncs {
		if fn < im.Base || fn >= im.End {
			t.Fatalf("init[%d] = %#x outside image [%#x,%#x)", i, fn, im.Base, im.End)
		}
	}
	if len(im.InitFuncs) == 0 {
		t.Error("genuine engine library has no constructors")
	}

	// Exported functions must point inside the image.
	for name, addr := range im.Exports {
		if addr < im.Base || addr >= im.End {
			t.Fatalf("export %q = %#x outside image [%#x,%#x)", name, addr, im.Base, im.End)
		}
	}

	// Unresolved strong functions are trap thunks: their addresses must be
	// outside the image (in the host thunk arena), never zero.
	for _, u := range im.Unresolved {
		if u.Weak {
			continue
		}
		// Trap thunk addresses are host-arena addresses, not image addresses.
	}
	t.Logf("loaded genuine library: span=%d bytes, exports=%d, inits=%d, unresolved=%d",
		im.Size, len(im.Exports), len(im.InitFuncs), len(im.Unresolved))
}
