package engine

import (
	"encoding/binary"
	"testing"
)

// buildVerneed builds a synthetic .gnu.version_r payload.
//
//	record 1: libc.so, 2 aux entries -> index 3 "LIBC", index 5 "LIBC_O"
//	record 2: libdl.so, 1 aux entry  -> index 7 "LIBC"
func buildVerneed() (verneed, strtab []byte) {
	str := &strBuilder{}
	libc := str.add("libc.so")
	libdl := str.add("libdl.so")
	li := str.add("LIBC")
	liO := str.add("LIBC_O")

	v := make([]byte, 0, 16+16+16+16+16)
	put16 := func(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
	put32 := func(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

	// Record 1 at offset 0, aux area at relative offset 16 (right after the
	// 16-byte record header).
	rec1 := make([]byte, 16)
	put16(rec1, 0, 1)            // vn_version
	put16(rec1, 2, 2)            // vn_cnt
	put32(rec1, 4, uint32(libc)) // vn_file
	put32(rec1, 8, 16)           // vn_aux
	put32(rec1, 12, 48)          // vn_next -> 48
	v = append(v, rec1...)

	aux1 := make([]byte, 16)
	put16(aux1, 4, 1)          // vna_flags
	put16(aux1, 6, 3)          // vna_other -> index 3
	put32(aux1, 8, uint32(li)) // vna_name
	put32(aux1, 12, 16)        // vna_next
	v = append(v, aux1...)

	aux2 := make([]byte, 16)
	put16(aux2, 4, 1)           // vna_flags
	put16(aux2, 6, 5)           // vna_other -> index 5
	put32(aux2, 8, uint32(liO)) // vna_name
	put32(aux2, 12, 0)          // vna_next (last)
	v = append(v, aux2...)

	// Record 2 at offset 48.
	rec2 := make([]byte, 16)
	put16(rec2, 0, 1)             // vn_version
	put16(rec2, 2, 1)             // vn_cnt
	put32(rec2, 4, uint32(libdl)) // vn_file
	put32(rec2, 8, 16)            // vn_aux
	put32(rec2, 12, 0)            // vn_next (last record)
	v = append(v, rec2...)

	aux3 := make([]byte, 16)
	put16(aux3, 4, 1)          // vna_flags
	put16(aux3, 6, 7)          // vna_other -> index 7
	put32(aux3, 8, uint32(li)) // vna_name
	put32(aux3, 12, 0)         // vna_next
	v = append(v, aux3...)

	return v, str.buf()
}

// buildVerdef builds a synthetic .gnu.version_d payload with one version
// definition: index 2, "LIBROBLOX".
func buildVerdef() (verdef, strtab []byte) {
	str := &strBuilder{}
	nm := str.add("LIBROBLOX")

	v := make([]byte, 20+8)
	put16 := func(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
	put32 := func(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

	put16(v, 0, 1)           // vd_version
	put16(v, 2, 1)           // vd_flags (VER_FLG_BASE)
	put16(v, 4, 2)           // vd_ndx -> index 2
	put16(v, 6, 1)           // vd_cnt
	put32(v, 12, 20)         // vd_aux
	put32(v, 16, 0)          // vd_next
	put32(v, 20, uint32(nm)) // vda_name
	put32(v, 24, 0)          // vda_next
	return v, str.buf()
}

func TestDecodeVerneedAndVerdef(t *testing.T) {
	b := &Binary{Versions: map[uint16]VersionInfo{}}

	vn, str := buildVerneed()
	if err := b.decodeVerneed(vn, str); err != nil {
		t.Fatal(err)
	}

	wantN := map[uint16]VersionInfo{
		3: {Library: "libc.so", Name: "LIBC"},
		5: {Library: "libc.so", Name: "LIBC_O"},
		7: {Library: "libdl.so", Name: "LIBC"},
	}
	for idx, want := range wantN {
		got, ok := b.Versions[idx]
		if !ok {
			t.Errorf("version index %d missing", idx)
			continue
		}
		if got != want {
			t.Errorf("version index %d = %+v, want %+v", idx, got, want)
		}
	}
	if len(b.Versions) != 3 {
		t.Errorf("verneed produced %d versions, want 3", len(b.Versions))
	}

	vd, str := buildVerdef()
	if err := b.decodeVerdef(vd, str); err != nil {
		t.Fatal(err)
	}
	got, ok := b.Versions[2]
	if !ok {
		t.Fatal("verdef index 2 missing")
	}
	if got.Name != "LIBROBLOX" || got.Library != "" {
		t.Errorf("verdef index 2 = %+v, want {LIBROBLOX, no library}", got)
	}
	if len(b.Versions) != 4 {
		t.Errorf("after verdef: %d versions, want 4", len(b.Versions))
	}
}

// strBuilder assembles a minimal dynamic string table.
type strBuilder struct {
	bytes []byte
}

func (s *strBuilder) add(str string) int {
	if len(s.bytes) == 0 {
		s.bytes = append(s.bytes, 0) // index 0 = empty string
	}
	off := len(s.bytes)
	s.bytes = append(s.bytes, str...)
	s.bytes = append(s.bytes, 0)
	return off
}

func (s *strBuilder) buf() []byte { return s.bytes }
