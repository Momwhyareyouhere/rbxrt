// Package engine analyses Roblox's native Android binaries and prepares them
// for loading on Linux. Nothing here executes code: it reads ELF structure,
// enumerates imports and exports, and plans how every imported symbol will be
// provided (host runtime, Android-platform shim, or a loud stub).
package engine

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ELF symbol binding/type halves of st_info.
const (
	stbLocal  = 0
	stbGlobal = 1
	stbWeak   = 2
)

const (
	sttNotype = 0
	sttObject = 1
	sttFunc   = 2
)

// Android-specific dynamic tags not named by debug/elf.
const (
	dtAndroidRel  = 0x60000011 // DT_ANDROID_REL  — packed (LEB128) REL relocations
	dtAndroidRela = 0x60000012 // DT_ANDROID_RELA — packed (LEB128) RELA relocations
	dtAndroidRelr = 0x6fffe000 // DT_ANDROID_RELR — RELR (sparse) relative relocs
)

// OSABI value for Android binaries (ELFOSABI_ANDROID); debug/elf does not
// export it.
const osabiAndroid = 84

// Android-specific section types missing from debug/elf. The Android linker
// reserves the low OS-specific range: SHT_ANDROID_REL=0x60000001,
// SHT_ANDROID_RELA=0x60000002 (binutils' elf/common.h and AOSP <elf.h> agree),
// and RELR lives just below the GNU range.
const (
	shtAndroidRel  = 0x60000001
	shtAndroidRela = 0x60000002
	shtAndroidRelr = 0x6ff00001
	shtRelrBase    = 0x6fffff00 // SHT_RELR
)

// Binary is a parsed Roblox-adjacent ELF shared object.
type Binary struct {
	Path string
	File *elf.File

	Class     string // "ELF64"
	Arch      string // e.g. "x86_64", "AArch64"
	Endian    string
	OSABI     string
	BuildID   string // hex from .note.gnu.build-id
	AndroidID int    // target Android version from .note.android.ident (0 if none)
	NDK       string // NDK / toolchain string from the note

	SOName string
	Needed []string

	Imports []*Symbol // undefined dynamic symbols
	Exports []*Symbol // defined global/weak dynamic symbols

	// SymByIndex maps a dynamic-symbol index (as used by relocation r_info)
	// to the parsed symbol, whether import, export, or defined-local.
	SymByIndex map[int]*Symbol

	// Relocations as recorded in the dynamic section.
	Relocs RelocationSet

	Versym   []uint16
	Versions map[uint16]VersionInfo // index -> library/name (indices >=2)

	relocInfo dynamic // DT_* snapshot (filled by parseDynamic)
}

// VersionInfo describes one symbol version: which library exports it and its
// version name ("LIBC_N", "LIBROBLOX", …).
type VersionInfo struct {
	Library string // e.g. "libc.so"
	Name    string // e.g. "LIBC_N"; "" when anonymous
}

// Symbol is one dynamic symbol, together with the classification the runtime
// assigns to an import.
type Symbol struct {
	Name       string
	Version    string
	VersionLib string
	Type       string // "FUNC", "OBJECT", "NOTYPE", …
	Bind       string // "GLOBAL", "WEAK", "LOCAL"
	Value      uint64
	Size       uint64

	Import bool

	// Classification (for imports).
	Class    ImportClass
	Provider string // library that will supply the symbol ("" = unresolved)
}

// ImportClass says how the runtime intends to resolve an imported symbol.
type ImportClass int

const (
	ClassUnclassified ImportClass = iota
	ClassHost                     // supplied by the runtime's own bionic-compatible host
	ClassAndroidLib               // supplied by a bundled Android platform-lib shim
	ClassStub                     // no implementation; must fail loudly if called
)

func (c ImportClass) String() string {
	switch c {
	case ClassHost:
		return "host"
	case ClassAndroidLib:
		return "android-lib"
	case ClassStub:
		return "stub"
	default:
		return "unclassified"
	}
}

// Open parses the ELF at path.
func Open(path string) (*Binary, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	b := &Binary{Path: path, File: f}
	if err := b.parse(); err != nil {
		f.Close()
		return nil, err
	}
	return b, nil
}

// Close releases the underlying file.
func (b *Binary) Close() error { return b.File.Close() }

func (b *Binary) parse() error {
	if err := b.parseHeader(); err != nil {
		return err
	}
	if err := b.parseNotes(); err != nil {
		return err
	}
	if err := b.parseDynamic(); err != nil {
		return err
	}
	if err := b.parseSymVersions(); err != nil {
		return err
	}
	b.collectSymbols()
	if err := b.parseRelocations(); err != nil {
		return err
	}
	return nil
}

func (b *Binary) parseHeader() error {
	switch b.File.Class {
	case elf.ELFCLASS64:
		b.Class = "ELF64"
	case elf.ELFCLASS32:
		b.Class = "ELF32"
	default:
		return fmt.Errorf("engine: %s: unsupported ELF class", b.Path)
	}
	switch b.File.Machine {
	case elf.EM_X86_64:
		b.Arch = "x86_64"
	case elf.EM_AARCH64:
		b.Arch = "AArch64"
	case elf.EM_386:
		b.Arch = "i386"
	case elf.EM_ARM:
		b.Arch = "ARM"
	default:
		b.Arch = fmt.Sprintf("machine %#x", uint16(b.File.Machine))
	}
	b.Endian = "unknown"
	switch b.File.Data {
	case elf.ELFDATA2LSB:
		b.Endian = "little"
	case elf.ELFDATA2MSB:
		b.Endian = "big"
	}
	switch b.File.OSABI {
	case elf.ELFOSABI_NONE:
		b.OSABI = "none"
	case elf.ELFOSABI_ARM:
		b.OSABI = "arm"
	case elf.ELFOSABI_LINUX:
		b.OSABI = "linux"
	case osabiAndroid:
		b.OSABI = "android"
	default:
		b.OSABI = fmt.Sprintf("osabi %#x", byte(b.File.OSABI))
	}
	// The NDK sets OSABI to SYSV (0); android-ness is carried by the
	// .note.android.ident section, so no OSABI requirement here.
	return nil
}

// parseNotes extracts .note.android.ident (target SDK + toolchain) and the GNU
// build id.
func (b *Binary) parseNotes() error {
	for _, s := range b.File.Sections {
		switch s.Name {
		case ".note.android.ident":
			data, err := s.Data()
			if err != nil {
				return fmt.Errorf("engine: reading notes: %w", err)
			}
			b.parseAndroidNote(data)
		case ".note.gnu.build-id":
			data, err := s.Data()
			if err != nil {
				return fmt.Errorf("engine: reading notes: %w", err)
			}
			b.BuildID = hexFromNote(data)
		}
	}
	return nil
}

// parseAndroidNote handles the NDK's Android note (NT_VERSION): a 32-bit
// version (the minimum target SDK / note format version stamped by the NDK),
// followed by four 32-byte string fields (NDK build string, build version,
// toolchain name, ABI type). Layouts have grown over time; we keep the version
// plus the concatenated non-empty strings.
func (b *Binary) parseAndroidNote(note []byte) {
	if len(note) < 12 {
		return
	}
	nSz := binary.LittleEndian.Uint32(note)
	dSz := binary.LittleEndian.Uint32(note[4:])
	if binary.LittleEndian.Uint32(note[8:]) != 1 { // NT_VERSION
		return
	}
	off := 12 + int(nSz)
	off = (off + 3) &^ 3
	if off+4 > len(note) {
		return
	}
	desc := note[off:]
	if int(dSz) < len(desc) {
		desc = desc[:int(dSz)]
	}
	b.AndroidID = int(int32(binary.LittleEndian.Uint32(desc)))
	var fields []string
	for i := 4; i+32 <= len(desc); i += 32 {
		f := strings.TrimRight(string(desc[i:i+32]), "\x00")
		if f != "" {
			fields = append(fields, f)
		}
	}
	b.NDK = strings.Join(fields, " ")
}

func hexFromNote(note []byte) string {
	// note: nameSize u32, descSize u32, type u32, name, pad4, desc
	if len(note) < 16 {
		return ""
	}
	nSz := binary.LittleEndian.Uint32(note)
	dSz := binary.LittleEndian.Uint32(note[4:])
	off := 12 + int(nSz)
	off = (off + 3) &^ 3
	if off+int(dSz) > len(note) {
		dSz = uint32(len(note) - off)
	}
	desc := note[off : off+int(dSz)]
	const hexd = "0123456789abcdef"
	out := make([]byte, len(desc)*2)
	for i, c := range desc {
		out[i*2] = hexd[c>>4]
		out[i*2+1] = hexd[c&0xf]
	}
	return string(out)
}

// dynstr reads the string table referenced by the dynamic section (SHT_STRTAB
// with sh_link = section index of the dynsym section, i.e. .dynstr).
func (b *Binary) dynstr() ([]byte, error) {
	sym := b.File.SectionByType(elf.SHT_DYNSYM)
	if sym == nil {
		return nil, fmt.Errorf("engine: %s: no dynamic symbol table", b.Path)
	}
	str := b.File.Sections[sym.Link]
	if str == nil {
		return nil, fmt.Errorf("engine: %s: dynsym has no linked string table", b.Path)
	}
	return str.Data()
}

func cstr(b []byte, off uint32) string {
	if int(off) >= len(b) {
		return ""
	}
	end := int(off)
	for end < len(b) && b[end] != 0 {
		end++
	}
	return string(b[off:end])
}

// ---- dynamic symbol collection ----

func (b *Binary) collectSymbols() {
	dynsym := b.File.SectionByType(elf.SHT_DYNSYM)
	if dynsym == nil {
		return
	}
	syms, _ := dynsym.Data()
	str, err := b.dynstr()
	if err != nil {
		return
	}
	ent := int(dynsym.Entsize)
	if ent == 0 {
		ent = 24
	}
	for i := 0; i+ent <= len(syms); i += ent {
		sv, ok := decodeSymbol(b, syms[i:i+ent])
		if !ok {
			continue
		}
		bind := sv.Info >> 4
		typ := sv.Info & 0xf
		idx := i / ent
		if idx == 0 {
			continue // dynsym[0] is the null symbol (empty name, SHN_UNDEF)
		}
		s := &Symbol{
			Name:   cstr(str, sv.NameOff),
			Type:   symbolTypeName(typ),
			Bind:   symbolBindName(bind),
			Value:  sv.Value,
			Size:   sv.Size,
			Import: sv.Index == 0, // SHN_UNDEF
		}
		if b.Versions != nil && idx < len(b.Versym) {
			v := b.Versym[idx] & 0x7fff
			if info, ok := b.Versions[v]; ok {
				s.Version = info.Name
				s.VersionLib = info.Library
			}
		}
		if s.Name == "" && !s.Import {
			continue
		}
		if b.SymByIndex == nil {
			b.SymByIndex = map[int]*Symbol{}
		}
		b.SymByIndex[idx] = s
		if s.Import {
			b.Imports = append(b.Imports, s)
		} else if bind == stbGlobal || bind == stbWeak {
			b.Exports = append(b.Exports, s)
		}
	}
	sort.Slice(b.Imports, func(i, j int) bool { return b.Imports[i].Name < b.Imports[j].Name })
	sort.Slice(b.Exports, func(i, j int) bool { return b.Exports[i].Name < b.Exports[j].Name })
}

type dynSymRaw struct {
	NameOff uint32
	Info    byte
	Index   uint16 // section index; 0 == undefined
	Value   uint64
	Size    uint64
}

func decodeSymbol(b *Binary, raw []byte) (dynSymRaw, bool) {
	if b.Class == "ELF64" {
		if len(raw) < 24 {
			return dynSymRaw{}, false
		}
		return dynSymRaw{
			NameOff: binary.LittleEndian.Uint32(raw[0:]),
			Info:    raw[4],
			Index:   binary.LittleEndian.Uint16(raw[6:]),
			Value:   binary.LittleEndian.Uint64(raw[8:]),
			Size:    binary.LittleEndian.Uint64(raw[16:]),
		}, true
	}
	if len(raw) < 16 {
		return dynSymRaw{}, false
	}
	return dynSymRaw{
		NameOff: binary.LittleEndian.Uint32(raw[0:]),
		Info:    raw[4],
		Index:   binary.LittleEndian.Uint16(raw[6:]),
		Value:   uint64(binary.LittleEndian.Uint32(raw[8:])),
		Size:    uint64(binary.LittleEndian.Uint32(raw[12:])),
	}, true
}

func symbolTypeName(t byte) string {
	switch t {
	case sttNotype:
		return "NOTYPE"
	case sttObject:
		return "OBJECT"
	case sttFunc:
		return "FUNC"
	case 6:
		return "TLS"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

func symbolBindName(b byte) string {
	switch b {
	case stbLocal:
		return "LOCAL"
	case stbGlobal:
		return "GLOBAL"
	case stbWeak:
		return "WEAK"
	default:
		return fmt.Sprintf("bind%d", b)
	}
}

// Summary prints a compact human-oriented report.
func (b *Binary) Summary(w io.Writer) {
	fmt.Fprintf(w, "%s\n", b.Path)
	fmt.Fprintf(w, "  format:   %s %s %s-endian, OSABI %s\n", b.Class, b.Arch, b.Endian, b.OSABI)
	fmt.Fprintf(w, "  android:  target SDK %d, NDK %q, build id %s\n", b.AndroidID, b.NDK, b.BuildID)
	fmt.Fprintf(w, "  soname:   %s\n", b.SOName)
	fmt.Fprintf(w, "  needed:   %s\n", strings.Join(b.Needed, ", "))
	fmt.Fprintf(w, "  symbols:  %d imports, %d exports\n", len(b.Imports), len(b.Exports))
}

var _ = os.Getenv
