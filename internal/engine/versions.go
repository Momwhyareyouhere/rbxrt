package engine

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// parseSymVersions builds the version-index map used to tag dynamic symbols
// with their version name and providing library. Sources are the three GNU
// version sections:
//
//	.gnu.version   (SHT_GNU_versym):  Elf32_Half per dynamic symbol.
//	.gnu.version_r (SHT_GNU_verneed): versions the DSO *needs* from others.
//	.gnu.version_d (SHT_GNU_verdef):  versions the DSO itself *defines*.
//
// Index space (as used by versym low 15 bits):
//
//	0       undefined (local)
//	1       global, unversioned
//	>= 2    defined by verdef (vd_ndx) or needed from verneed (vna_other)
func (b *Binary) parseSymVersions() error {
	str, err := b.dynstr()
	if err != nil {
		// Without .dynstr we cannot name versions; leave the table empty.
		return nil
	}

	// .gnu.version
	if s := b.File.Section(".gnu.version"); s != nil {
		data, err := s.Data()
		if err != nil {
			return fmt.Errorf("engine: reading .gnu.version: %w", err)
		}
		if len(data) >= 2 {
			b.Versym = make([]uint16, len(data)/2)
			for i := range b.Versym {
				b.Versym[i] = binary.LittleEndian.Uint16(data[i*2:])
			}
		}
	}

	b.Versions = make(map[uint16]VersionInfo)

	// .gnu.version_r — libraries we import versioned symbols from.
	if s := b.File.Section(".gnu.version_r"); s != nil {
		if err := b.parseVerneed(s, str); err != nil {
			return err
		}
	}

	// .gnu.version_d — version names this DSO defines (its export versions).
	if s := b.File.Section(".gnu.version_d"); s != nil {
		if err := b.parseVerdef(s, str); err != nil {
			return err
		}
	}
	return nil
}

func (b *Binary) parseVerneed(s *elf.Section, str []byte) error {
	data, err := s.Data()
	if err != nil {
		return err
	}
	return b.decodeVerneed(data, str)
}

// decodeVerneed parses raw .gnu.version_r data into b.Versions.
func (b *Binary) decodeVerneed(data, str []byte) error {
	// Elf32_/Elf64_Verneed: vn_version(2) vn_cnt(2) vn_file(4) vn_aux(4) vn_next(4)
	// Elf32_/Elf64_Vernaux: vna_hash(4) vna_flags(2) vna_other(2) vna_name(4) vna_next(4)
	// Both classes use identical 16-byte record layouts, so decoding is uniform.
	const vnh, vna = 16, 16
	off := 0
	for off+vnh <= len(data) {
		cnt := binary.LittleEndian.Uint16(data[off+2:])
		fileOff := binary.LittleEndian.Uint32(data[off+4:])
		auxOff := binary.LittleEndian.Uint32(data[off+8:])
		next := binary.LittleEndian.Uint32(data[off+12:])
		lib := cstr(str, fileOff)
		ao := off + int(auxOff)
		for i := uint16(0); i < cnt && ao+vna <= len(data); i++ {
			// Elf64_Vernaux: vna_hash(4) vna_flags(2) vna_other(2) vna_name(4) vna_next(4)
			other := binary.LittleEndian.Uint16(data[ao+6:])
			name := binary.LittleEndian.Uint32(data[ao+8:])
			nx := binary.LittleEndian.Uint32(data[ao+12:])
			if other >= 2 {
				b.Versions[other] = VersionInfo{Library: lib, Name: cstr(str, name)}
			}
			if nx == 0 {
				break
			}
			ao += int(nx)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return nil
}

func (b *Binary) parseVerdef(s *elf.Section, str []byte) error {
	data, err := s.Data()
	if err != nil {
		return err
	}
	return b.decodeVerdef(data, str)
}

// decodeVerdef parses raw .gnu.version_d data into b.Versions.
func (b *Binary) decodeVerdef(data, str []byte) error {
	// Elf32_/Elf64_Verdef: vd_version(2) vd_flags(2) vd_ndx(2) vd_cnt(2) vd_hash(4) vd_aux(4) vd_next(4)
	// Elf32_/Elf64_Verdaux: vda_name(4) vda_next(4)
	// Both classes use identical 20/8-byte record layouts, so decoding is uniform.
	const vdh, vda = 20, 8
	off := 0
	for off+vdh <= len(data) {
		ndx := binary.LittleEndian.Uint16(data[off+4:])
		cnt := binary.LittleEndian.Uint16(data[off+6:])
		auxOff := binary.LittleEndian.Uint32(data[off+12:])
		next := binary.LittleEndian.Uint32(data[off+16:])
		ao := off + int(auxOff)
		for i := uint16(0); i < cnt && ao+vda <= len(data); i++ {
			name := binary.LittleEndian.Uint32(data[ao:])
			nx := binary.LittleEndian.Uint32(data[ao+4:])
			if ndx >= 2 {
				b.Versions[ndx] = VersionInfo{Name: cstr(str, name)}
			}
			if nx == 0 {
				break
			}
			ao += int(nx)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return nil
}
