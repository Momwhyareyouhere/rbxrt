package engine

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// Relocation type numbers per architecture.
const (
	// x86_64
	rX64None      = 0
	rX64Abs64     = 1
	rX64GlobDat   = 6
	rX64JumpSlot  = 7
	rX64Relative  = 8
	rX64Irelative = 37
	// AArch64
	rA64None      = 0
	rA64Abs64     = 257
	rA64GlobDat   = 1025
	rA64JumpSlot  = 1026
	rA64Relative  = 1027
	rA64Irelative = 1032
	// i386
	r386None      = 0
	r386Abs32     = 1
	r386GlobDat   = 6
	r386JumpSlot  = 7
	r386Relative  = 8
	r386Irelative = 42
)

// Android packed relocation group flags (AOSP linker_reloc_iterators.h):
// RELOCATION_GROUPED_BY_INFO_FLAG = 1
// RELOCATION_GROUPED_BY_OFFSET_DELTA_FLAG = 2
// RELOCATION_GROUPED_BY_ADDEND_FLAG = 4
// RELOCATION_GROUP_HAS_ADDEND_FLAG = 8
const (
	packGroupByInfo        = 1
	packGroupByOffsetDelta = 2
	packGroupByAddend      = 4
	packGroupHasAddend     = 8
)

// RelocKind classifies a relocation by what the runtime must do with it.
type RelocKind int

const (
	RelocRelative  RelocKind = iota // add load bias (R_*_RELATIVE)
	RelocAbs                        // absolute symbol value + addend
	RelocGlobDat                    // store symbol address
	RelocJumpSlot                   // PLT slot: store symbol address
	RelocIrelative                  // call resolver function, store result
	RelocUnknown
)

func (k RelocKind) String() string {
	switch k {
	case RelocRelative:
		return "RELATIVE"
	case RelocAbs:
		return "ABS"
	case RelocGlobDat:
		return "GLOB_DAT"
	case RelocJumpSlot:
		return "JUMP_SLOT"
	case RelocIrelative:
		return "IRELATIVE"
	default:
		return "unknown"
	}
}

// Reloc is one runtime relocation.
type Reloc struct {
	Offset  uint64
	Kind    RelocKind
	Type    uint64 // raw ELF relocation type
	SymIdx  uint32 // dynamic symbol index, 0 == none
	Addend  int64
	Symbol  *Symbol // bound import, set after ClassifyImports
	Grouped bool    // produced by a group/skip encoding
}

// RelocationSet is the parsed relocation inventory of a DSO.
type RelocationSet struct {
	Relative []*Reloc // R_*_RELATIVE
	Other    []*Reloc // symbol-bearing relocations (and non-relative)
	Total    int
}

func (r *RelocationSet) hasData() bool { return r.Total > 0 }

// parseRelocations inventories every relocation table of the DSO. The
// authoritative source is the section table: the Android toolchain emits
// SHT_ANDROID_REL[A] (packed APR2/APS2 formats) and either SHT_RELA (.rela.plt)
// or packs everything. RELR sections are handled when present.
func (b *Binary) parseRelocations() error {
	var set RelocationSet
	for _, s := range b.File.Sections {
		switch uint32(s.Type) {
		case shtAndroidRela:
			if err := b.parsePacked(s, &set); err != nil {
				return fmt.Errorf("%s: packed rela: %w", s.Name, err)
			}
		case shtAndroidRel:
			if err := b.parsePacked(s, &set); err != nil {
				return fmt.Errorf("%s: packed rel: %w", s.Name, err)
			}
		case uint32(elf.SHT_RELA):
			if err := b.parsePlainRela(s, &set); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
		case uint32(elf.SHT_REL):
			if err := b.parsePlainRel(s, &set); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
		case shtRelrBase, shtAndroidRelr:
			if err := b.parseRelr(s, &set); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
		}
	}
	set.Total = len(set.Relative) + len(set.Other)
	b.Relocs = set
	return nil
}

// parsePacked decodes an Android packed relocation section ("APS2" magic plus a
// SLEB128 stream; the older APR2 magic is rejected — the APS2 layout is what
// current toolchains emit). Faithful port of AOSP
// linker/linker_reloc_iterators.h for_all_packed_relocs.
func (b *Binary) parsePacked(sec *elf.Section, set *RelocationSet) error {
	raw, err := sec.Data()
	if err != nil {
		return err
	}
	return b.decodePacked(raw, set)
}

// decodePacked decodes an APS2 packed-relocation payload (header excluded).
func (b *Binary) decodePacked(raw []byte, set *RelocationSet) error {
	if len(raw) < 4 || string(raw[:4]) != "APS2" {
		return fmt.Errorf("bad android relocation header (want APS2, got %q)", raw[:min(4, len(raw))])
	}
	d := slebDecoder{data: raw[4:]}

	n := d.pop()
	if n <= 0 {
		return nil
	}
	rOffset := d.pop() // initial r_offset value

	// r_offset, r_info and r_addend persist across groups, exactly as AOSP's
	// for_all_packed_relocs keeps a single `rel` across the whole stream.
	var rInfo, addend int64

	idx := int64(0)
	for idx < n {
		groupSize := d.pop()
		groupFlags := d.pop()
		if groupSize <= 0 {
			return fmt.Errorf("zero-size relocation group")
		}

		var gDelta int64
		if groupFlags&packGroupByOffsetDelta != 0 {
			gDelta = d.pop()
		}
		if groupFlags&packGroupByInfo != 0 {
			rInfo = d.pop()
		}

		af := groupFlags & (packGroupHasAddend | packGroupByAddend)
		switch af {
		case packGroupHasAddend | packGroupByAddend:
			// A single addend delta for the whole group, accumulated into the
			// running addend (AOSP: reloc.r_addend += pop_front()).
			addend += d.pop()
		case packGroupHasAddend:
			// every relocation carries its own addend delta
		default:
			addend = 0
		}

		for i := int64(0); i < groupSize; i++ {
			if groupFlags&packGroupByOffsetDelta != 0 {
				rOffset += gDelta
			} else {
				rOffset += d.pop()
			}
			if groupFlags&packGroupByInfo == 0 {
				rInfo = d.pop()
			}
			if af == packGroupHasAddend {
				addend += d.pop()
			}
			set.addReloc(b, uint64(rOffset), uint64(rInfo), addend, true)
		}
		idx += groupSize
	}
	return nil
}

// parsePlainRela reads an ordinary RELA table.
func (b *Binary) parsePlainRela(sec *elf.Section, set *RelocationSet) error {
	raw, err := sec.Data()
	if err != nil {
		return err
	}
	ent := sec.Entsize
	if ent == 0 {
		ent = 24
	}
	for i := 0; i+int(ent) <= len(raw); i += int(ent) {
		off := binary.LittleEndian.Uint64(raw[i:])
		info := binary.LittleEndian.Uint64(raw[i+8:])
		add := int64(binary.LittleEndian.Uint64(raw[i+16:]))
		set.addReloc(b, off, info, add, false)
	}
	return nil
}

// parsePlainRel reads an ordinary REL table.
func (b *Binary) parsePlainRel(sec *elf.Section, set *RelocationSet) error {
	raw, err := sec.Data()
	if err != nil {
		return err
	}
	ent := sec.Entsize
	if ent == 0 {
		ent = 8
	}
	for i := 0; i+int(ent) <= len(raw); i += int(ent) {
		var off, info uint64
		if b.Class == "ELF64" {
			off = binary.LittleEndian.Uint64(raw[i:])
			info = binary.LittleEndian.Uint64(raw[i+8:])
		} else {
			off = uint64(binary.LittleEndian.Uint32(raw[i:]))
			info = uint64(binary.LittleEndian.Uint32(raw[i+4:]))
		}
		set.addReloc(b, off, info, 0, false)
	}
	return nil
}

// parseRelr decodes a RELR stream: an "even word" establishes a base address,
// and each following "odd word" is a bitmap in which every set bit j (except
// bit 0, the marker) addresses base + j*wordsize. Port of AOSP
// linker/linker_relr.cpp relocate_relr.
func (b *Binary) parseRelr(sec *elf.Section, set *RelocationSet) error {
	raw, err := sec.Data()
	if err != nil {
		return err
	}
	ent := sec.Entsize
	word := 8
	if b.Class == "ELF32" {
		word = 4
	}
	if ent == 0 {
		ent = uint64(word)
	}
	var base uint64
	for i := 0; i+int(ent) <= len(raw); i += int(ent) {
		var v uint64
		if word == 8 {
			v = binary.LittleEndian.Uint64(raw[i:])
		} else {
			v = uint64(binary.LittleEndian.Uint32(raw[i:]))
		}
		if v&1 != 0 {
			base = v &^ 1
			continue
		}
		for j := uint(1); j < uint(word*8); j++ {
			if v&(1<<j) != 0 {
				set.addReloc(b, base+uint64(j)*uint64(word), uint64(b.relTypeRelative()), 0, true)
			}
		}
	}
	return nil
}

// addReloc folds one decoded relocation into the set.
func (s *RelocationSet) addReloc(b *Binary, off, info uint64, addend int64, grouped bool) {
	sym := uint32(info >> 32)
	typ := uint32(info & 0xffffffff)
	r := &Reloc{
		Offset: off, Kind: b.relocKind(typ), Type: uint64(typ),
		SymIdx: sym, Addend: addend, Grouped: grouped,
	}
	if sym == 0 && r.Kind == RelocRelative {
		s.Relative = append(s.Relative, r)
	} else {
		s.Other = append(s.Other, r)
	}
	s.Total++
}

// relocKind maps an architecture-specific ELF relocation type to the generic
// kind the runtime cares about.
func (b *Binary) relocKind(t uint32) RelocKind {
	switch b.Arch {
	case "x86_64":
		switch t {
		case rX64Relative:
			return RelocRelative
		case rX64JumpSlot:
			return RelocJumpSlot
		case rX64GlobDat:
			return RelocGlobDat
		case rX64Abs64:
			return RelocAbs
		case rX64Irelative:
			return RelocIrelative
		}
	case "AArch64":
		switch t {
		case rA64Relative:
			return RelocRelative
		case rA64JumpSlot:
			return RelocJumpSlot
		case rA64GlobDat:
			return RelocGlobDat
		case rA64Abs64:
			return RelocAbs
		case rA64Irelative:
			return RelocIrelative
		}
	case "i386":
		switch t {
		case r386Relative:
			return RelocRelative
		case r386JumpSlot:
			return RelocJumpSlot
		case r386GlobDat:
			return RelocGlobDat
		case r386Abs32:
			return RelocAbs
		case r386Irelative:
			return RelocIrelative
		}
	}
	return RelocUnknown
}

// relTypeRelative returns the R_*_RELATIVE numeric type for the architecture.
func (b *Binary) relTypeRelative() uint32 {
	switch b.Arch {
	case "AArch64":
		return rA64Relative
	case "i386":
		return r386Relative
	default:
		return rX64Relative
	}
}

// slebDecoder is a signed LEB128 reader matching bionic's sleb128_decoder.
type slebDecoder struct {
	data []byte
	pos  int
}

func (d *slebDecoder) pop() (val int64) {
	var shift uint
	for {
		if d.pos >= len(d.data) {
			panic("sleb128 run out of bounds")
		}
		byte := d.data[d.pos]
		d.pos++
		val |= int64(byte&0x7f) << shift
		shift += 7
		if byte&0x80 == 0 {
			break
		}
	}
	if shift < 64 && (d.data[d.pos-1]&0x40) != 0 {
		val |= -1 << shift
	}
	return val
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
