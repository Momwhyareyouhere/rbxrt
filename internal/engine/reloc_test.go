package engine

import (
	"bytes"
	"testing"
)

// sleb128 encodes a signed value in SLEB128 form (test helper; the decoder is
// the code under test, the encoder is the oracle).
func sleb128(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		sign := b & 0x40
		more := !((v == 0 && sign == 0) || (v == -1 && sign != 0))
		if more {
			b |= 0x80
		}
		out = append(out, b)
		if !more {
			return out
		}
	}
}

func TestSlebDecoder(t *testing.T) {
	cases := []int64{0, 1, -1, 63, 64, -63, -64, -65, 127, 128, -127, -128,
		0x1000, -0x1000, 572132, -627, 1 << 32, -(1 << 40), 0x1ef04d, -0x16020}
	for _, want := range cases {
		d := slebDecoder{data: sleb128(want)}
		if got := d.pop(); got != want {
			t.Errorf("sleb128(%d) = %d, want %d", want, got, want)
		}
	}
}

// packRelocInfo folds sym<<32|type like ELF64_R_INFO.
func packRelocInfo(sym, typ uint64) int64 { return int64(sym<<32 | typ) }

// TestDecodePacked covers the four flag combinations exercised by real bionic
// toolchains: grouped-by-info, offset deltas, common addend, per-reloc addend.
func TestDecodePacked(t *testing.T) {
	b := &Binary{Arch: "x86_64"}

	rel := func(sym, typ uint64) int64 { return packRelocInfo(sym, typ) }

	// num_relocs = 6, initial r_offset = 0x10000
	payload := &bytes.Buffer{}
	payload.WriteString("APS2")
	payload.Write(sleb128(6))
	payload.Write(sleb128(0x10000))

	// Group 1: by-info | by-offset-delta (flags=3), size 2, no addends.
	//   gDelta=0x20, rInfo = R_X86_64_RELATIVE (8, sym 0)
	payload.Write(sleb128(2))
	payload.Write(sleb128(packGroupByInfo | packGroupByOffsetDelta))
	payload.Write(sleb128(0x20))
	payload.Write(sleb128(rel(0, rX64Relative)))

	// Group 2: by-info | has-addend | by-addend (flags=13), size 2.
	//   rInfo (by-info) is written before the common addend, per the AOSP
	//   packer. Common addend -8, rInfo = R_X86_64_GLOB_DAT (6, sym 1).
	//   Per-reloc offset deltas are popped.
	payload.Write(sleb128(2))
	payload.Write(sleb128(packGroupByInfo | packGroupHasAddend | packGroupByAddend))
	payload.Write(sleb128(rel(1, rX64GlobDat)))
	payload.Write(sleb128(-8))
	payload.Write(sleb128(0x8))
	payload.Write(sleb128(0x10))

	// Group 3: by-offset-delta | has-addend (flags=10), size 2. Per-reloc
	// bodies are interleaved [rInfo][addend_delta].
	payload.Write(sleb128(2))
	payload.Write(sleb128(packGroupByOffsetDelta | packGroupHasAddend))
	payload.Write(sleb128(0x10))
	payload.Write(sleb128(rel(2, rX64Abs64)))
	payload.Write(sleb128(0)) // addend delta for reloc 0
	payload.Write(sleb128(rel(0, rX64Relative)))
	payload.Write(sleb128(100)) // addend delta for reloc 1

	var set RelocationSet
	if err := b.decodePacked(payload.Bytes(), &set); err != nil {
		t.Fatal(err)
	}
	if got := set.Total; got != 6 {
		t.Fatalf("total relocs = %d, want 6", got)
	}

	// Relative bucket: group1 offset marches 0x10020 -> 0x10040 (each reloc adds
	// the group delta to the running r_offset), then group3's RELATIVE (sym 0)
	// lands here too, with addend 100.
	if len(set.Relative) != 3 {
		t.Fatalf("relative relocs = %d, want 3", len(set.Relative))
	}
	wantRel := []struct {
		off    uint64
		addend int64
	}{{0x10020, 0}, {0x10040, 0}, {0x10078, 100}}
	for i, w := range wantRel {
		if set.Relative[i].Offset != w.off {
			t.Errorf("relative[%d] offset = %#x, want %#x", i, set.Relative[i].Offset, w.off)
		}
		if set.Relative[i].Addend != w.addend {
			t.Errorf("relative[%d] addend = %d, want %d", i, set.Relative[i].Addend, w.addend)
		}
	}

	// Group 2 produces two GLOB_DAT(sym1) at 0x10048/0x10058 with common
	// addend -8.
	gd := findRelocs(set.Other, RelocGlobDat, 1)
	if len(gd) != 2 {
		t.Fatalf("GLOB_DAT(sym1) count = %d, want 2", len(gd))
	}
	for _, r := range gd {
		if r.Addend != -8 {
			t.Errorf("GLOB_DAT addend = %d, want -8", r.Addend)
		}
	}
	if gd[0].Offset != 0x10048 || gd[1].Offset != 0x10058 {
		t.Errorf("GLOB_DAT offsets = %#x,%#x want 0x10048,0x10058", gd[0].Offset, gd[1].Offset)
	}

	// Group 3 reloc 0: R_X86_64_64 (ABS) sym2, addend 0, at 0x10068.
	abs := findRelocs(set.Other, RelocAbs, 2)
	if len(abs) != 1 {
		t.Fatalf("ABS(sym2) count = %d, want 1", len(abs))
	}
	if abs[0].Offset != 0x10068 {
		t.Errorf("ABS offset = %#x, want 0x10068", abs[0].Offset)
	}
	if abs[0].Addend != 0 {
		t.Errorf("ABS addend = %d, want 0", abs[0].Addend)
	}
}

func findRelocs(relocs []*Reloc, kind RelocKind, sym uint32) []*Reloc {
	var out []*Reloc
	for _, r := range relocs {
		if r.Kind == kind && r.SymIdx == sym {
			out = append(out, r)
		}
	}
	return out
}

func TestDecodePackedRejectsLegacyHeaders(t *testing.T) {
	b := &Binary{Arch: "x86_64"}
	for _, hdr := range []string{"APR1", "APR2", "XXXX", "AP"} {
		var set RelocationSet
		if err := b.decodePacked([]byte(hdr), &set); err == nil {
			t.Errorf("header %q: expected error, got none", hdr)
		}
	}
}

func TestDecodePackedGroupSafety(t *testing.T) {
	b := &Binary{Arch: "x86_64"}
	p := &bytes.Buffer{}
	p.WriteString("APS2")
	p.Write(sleb128(1))
	p.Write(sleb128(0x1000))
	p.Write(sleb128(-1))              // invalid (negative) group size
	p.Write(sleb128(packGroupByInfo)) //
	if err := b.decodePacked(p.Bytes(), &RelocationSet{}); err == nil {
		t.Error("negative group size must error")
	}
}
