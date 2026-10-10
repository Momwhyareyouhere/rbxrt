package engine

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// dynamic is a snapshot of the DT_* entries the runtime cares about, kept for
// cross-checks and reporting. The authoritative location of relocation tables
// is the section headers (see parseRelocations); the dynamic entries let us
// sanity-check e.g. the JUMP_SLOT table size.
type dynamic struct {
	rela     uint64 // DT_RELA addr
	relasz   uint64
	relaent  uint64
	relacnt  uint64
	jmprel   uint64
	pltrelsz uint64
	pltrel   uint64 // DT_PLTREL: 7 == DT_RELA, 17 == DT_REL
	init     uint64 // DT_INIT
	fini     uint64 // DT_FINI
	initarr  uint64 // DT_INIT_ARRAY
	initasz  uint64 // DT_INIT_ARRAYSZ (bytes)
	finiarr  uint64 // DT_FINI_ARRAY
	finiasz  uint64 // DT_FINI_ARRAYSZ (bytes)
	gnuhash  uint64
	sysvhash uint64
	strsz    uint64
}

// parseDynamic reads the SHT_DYNAMIC section, filling b.Needed and b.SOName and
// snapshotting the relocation-related entries.
func (b *Binary) parseDynamic() error {
	dyn := b.File.SectionByType(elf.SHT_DYNAMIC)
	if dyn == nil {
		return fmt.Errorf("engine: %s: not a shared object (no dynamic section)", b.Path)
	}
	d, err := dyn.Data()
	if err != nil {
		return fmt.Errorf("engine: reading dynamic section: %w", err)
	}
	str, err := b.dynstr()
	if err != nil {
		return err
	}

	var dt dynamic
	ent := int(dyn.Entsize)
	if ent == 0 {
		ent = 16
	}
	for i := 0; i+ent <= len(d); i += ent {
		var tag, val uint64
		if b.Class == "ELF64" {
			tag = binary.LittleEndian.Uint64(d[i:])
			val = binary.LittleEndian.Uint64(d[i+8:])
		} else {
			tag = uint64(binary.LittleEndian.Uint32(d[i:]))
			val = uint64(binary.LittleEndian.Uint32(d[i+4:]))
		}
		// DT_* constants (values are stable across 32/64-bit).
		switch tag {
		case 1: // DT_NEEDED
			b.Needed = append(b.Needed, cstr(str, uint32(val)))
		case 2: // DT_PLTRELSZ
			dt.pltrelsz = val
		case 4: // DT_HASH
			dt.sysvhash = val
		case 5: // DT_STRTAB
		case 6: // DT_SYMTAB
		case 7: // DT_RELA
			dt.rela = val
		case 8: // DT_RELASZ
			dt.relasz = val
		case 9: // DT_RELAENT
			dt.relaent = val
		case 14: // DT_SONAME
			b.SOName = cstr(str, uint32(val))
		case 12: // DT_INIT
			dt.init = val
		case 13: // DT_FINI
			dt.fini = val
		case 16: // DT_JMPREL
			dt.jmprel = val
		case 17: // DT_BIND_NOW
		case 25: // DT_INIT_ARRAY
			dt.initarr = val
		case 26: // DT_FINI_ARRAY
			dt.finiarr = val
		case 27: // DT_INIT_ARRAYSZ
			dt.initasz = val
		case 28: // DT_FINI_ARRAYSZ
			dt.finiasz = val
		case 0x6ffffff9: // DT_RELACOUNT
			dt.relacnt = val
		case 47, 0x6ffffef5: // DT_FLAGS_1, DT_GNU_HASH
			if tag == 0x6ffffef5 {
				dt.gnuhash = val
			}
		}
	}
	b.relocInfo = dt
	return nil
}

// DynamicInit reports the constructor entry points recorded in the dynamic
// section: DT_INIT (if non-zero) and the DT_INIT_ARRAY range. All values are
// virtual addresses before the load bias is applied.
func (b *Binary) DynamicInit() (init uint64, initArray, initArraySize uint64) {
	return b.relocInfo.init, b.relocInfo.initarr, b.relocInfo.initasz
}

// DynamicFini reports the destructor entry points: DT_FINI and DT_FINI_ARRAY.
func (b *Binary) DynamicFini() (fini uint64, finiArray, finiArraySize uint64) {
	return b.relocInfo.fini, b.relocInfo.finiarr, b.relocInfo.finiasz
}
