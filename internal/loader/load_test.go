package loader

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"rbxrt/internal/engine"
	"rbxrt/internal/hostabi"
	"unsafe"
)

// buildFixture synthesises a minimal but valid ELF64 x86-64 DSO exercising
// every relocation kind and symbol class the loader handles. It is not
// something the engine needs to pretty-print; it exists to give the loader a
// controlled load surface.
//
// Image layout (guest vaddrs):
//
//	0x1000 .text        RX   (PT_LOAD, off 0x1000)
//	0x3000 .data/.got   RW   (PT_LOAD, off 0x2000, filesz 0x500, memsz 0x600)
//	0x3000 RELRO        R    (PT_GNU_RELRO, file range of .data)
//	0x3080 RELATIVE slot           -> bias + 0x1234
//	0x3088 GLOB_DAT host_strlen     -> strlen thunk
//	0x3090 GLOB_DAT __stack_chk_guard -> data object
//	0x3098 ABS64 guest_func         -> bias + 0x1000
//	0x30a0 JUMP_SLOT host_strlen    -> strlen thunk
//
// Symbols: guest_func (defined FUNC @0x1000), host_strlen (import), and
// __stack_chk_guard (import OBJECT). DT_INIT = 0x1050.
type fixtureOpts struct {
	// unresObj makes the fixture reference an undefined strong OBJECT so that
	// Load must fail hard.
	unresObj bool
}

func buildFixture(t *testing.T, opts fixtureOpts) string {
	t.Helper()

	var le = binary.LittleEndian

	// dynstr layout.
	dynstr := []byte{0}
	add := func(s string) uint32 {
		off := uint32(len(dynstr))
		dynstr = append(dynstr, []byte(s)...)
		dynstr = append(dynstr, 0)
		return off
	}
	oGuest := add("guest_func")
	oHost := add("strlen")
	oGuard := add("__stack_chk_guard")
	oLocal := add("local_obj")
	oLibc := add("libc.so")

	// dynsym.
	var dynsym []byte
	sym := func(name uint32, info byte, shndx uint16, value, size uint64) {
		ent := make([]byte, 24)
		le.PutUint32(ent[0:], name)
		ent[4] = info
		le.PutUint16(ent[6:], shndx)
		le.PutUint64(ent[8:], value)
		le.PutUint64(ent[16:], size)
		dynsym = append(dynsym, ent...)
	}
	sym(0, 0, 0, 0, 0)                 // null
	sym(oGuest, 0x12, 1, 0x1000, 0)    // GLOBAL FUNC defined
	sym(oHost, 0x12, 0, 0, 0)          // GLOBAL FUNC undefined
	sym(oGuard, 0x11, 0, 0, 0)         // GLOBAL OBJECT undefined
	sym(oLocal, 0x11, 6, 0x3100, 0x10) // GLOBAL OBJECT defined in .data

	if opts.unresObj {
		oNoSuch := add("nosuch_data_object")
		sym(oNoSuch, 0x11, 0, 0, 0)
	}

	// relocations (.rela.dyn: RELATIVE + 3 symbol-bearing; .rela.plt: 1).
	var relaDyn, relaPlt []byte
	rela := func(rOffset uint64, symIdx, typ uint32, addend int64, dst *[]byte) {
		b := make([]byte, 24)
		le.PutUint64(b[0:], rOffset)
		le.PutUint64(b[8:], uint64(symIdx)<<32|uint64(typ))
		le.PutUint64(b[16:], uint64(addend))
		*dst = append(*dst, b...)
	}
	rela(0x3080, 0, 8 /* RELATIVE */, 0x1234, &relaDyn)
	rela(0x3088, 2, 6 /* GLOB_DAT */, 0, &relaDyn)
	rela(0x3090, 3, 6 /* GLOB_DAT */, 0, &relaDyn)
	if opts.unresObj {
		rela(0x30b0, 5, 6 /* GLOB_DAT */, 0, &relaDyn)
	} else {
		rela(0x3098, 1, 1 /* ABS64 */, 0, &relaDyn)
	}
	rela(0x30a0, 2, 7 /* JUMP_SLOT */, 0, &relaPlt)

	// dynamic entries.
	dynsymAddr := uint64(0x3400)
	dynstrAddr := uint64(0x3600)
	relaDynAddr := uint64(0x3300)
	relaPltAddr := uint64(0x3500)
	dynamic := []uint64{
		1, uint64(oLibc), // DT_NEEDED libc.so
		5, dynstrAddr, // DT_STRTAB
		6, dynsymAddr, // DT_SYMTAB
		10, uint64(len(dynstr)), // DT_STRSZ
		11, 24, // DT_SYMENT
		7, relaDynAddr, // DT_RELA
		8, uint64(len(relaDyn)), // DT_RELASZ
		9, 24, // DT_RELAENT
		23, relaPltAddr, // DT_JMPREL
		2, uint64(len(relaPlt)), // DT_PLTRELSZ
		20, 7, // DT_PLTREL (RELA)
		12, 0x1050, // DT_INIT
		0, 0, // DT_NULL
	}
	var dynBuf []byte
	for i := 0; i < len(dynamic); i += 2 {
		e := make([]byte, 16)
		le.PutUint64(e[0:], dynamic[i])
		le.PutUint64(e[8:], dynamic[i+1])
		dynBuf = append(dynBuf, e...)
	}

	// .data payload.
	data := make([]byte, 0x600)
	// GOT slots zero-initialised; relocations will fill them in.

	// shstrtab.
	shstr := []byte{0}
	shadd := func(s string) uint32 {
		off := uint32(len(shstr))
		shstr = append(shstr, []byte(s)...)
		shstr = append(shstr, 0)
		return off
	}
	nDynsym := shadd(".dynsym")
	nDynstr := shadd(".dynstr")
	nRelaDyn := shadd(".rela.dyn")
	nRelaPlt := shadd(".rela.plt")
	nText := shadd(".text")
	nData := shadd(".data")
	nDynamic := shadd(".dynamic")
	nShstr := shadd(".shstrtab")

	// Section headers. Indexes: 0 null,1 dynsym,2 dynstr,3 rela.dyn,4 rela.plt,
	// 5 text, 6 data, 7 dynamic, 8 shstrtab.
	type shdr struct {
		name, typ              uint32
		flags, addr, off, size uint64
		link, info             uint32
		align, entsize         uint64
	}
	shdrs := []shdr{
		{},
		{nDynsym, 11, 0, 0, 0x3400, uint64(len(dynsym)), 2, 1, 8, 24},
		{nDynstr, 3, 0, 0, 0x3600, uint64(len(dynstr)), 0, 0, 1, 0},
		{nRelaDyn, 4, 0, 0, 0x3000, uint64(len(relaDyn)), 1, 0, 8, 24},
		{nRelaPlt, 4, 0, 0, 0x3200, uint64(len(relaPlt)), 1, 2, 8, 24},
		{nText, 1, 6, 0x1000, 0x1000, 0x100, 0, 0, 16, 0},
		{nData, 1, 3, 0x3000, 0x2000, 0x500, 0, 0, 8, 0},
		{nDynamic, 6, 3, 0x3280, 0x2800, uint64(len(dynBuf)), 2, 0, 8, 16},
		{nShstr, 3, 0, 0, 0x3800, uint64(len(shstr)), 0, 0, 1, 0},
	}

	shoff := uint64(0x3a00)
	fileSize := int(shoff) + len(shdrs)*64

	file := make([]byte, fileSize)

	// ELF header.
	w := func(off int, b []byte) { copy(file[off:], b) }
	w(0, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}) // 64-bit LSB, EV_CURRENT, SYSV
	le.PutUint16(file[16:], 3)                    // ET_DYN
	le.PutUint16(file[18:], 62)                   // EM_X86_64
	le.PutUint32(file[20:], 1)                    // version
	le.PutUint64(file[32:], 0x40)                 // e_phoff
	le.PutUint64(file[40:], shoff)                // e_shoff
	le.PutUint16(file[52:], 64)                   // e_ehsize
	le.PutUint16(file[54:], 56)                   // e_phentsize
	le.PutUint16(file[56:], 3)                    // e_phnum (2 load + relro)
	le.PutUint16(file[58:], 64)                   // e_shentsize
	le.PutUint16(file[60:], uint16(len(shdrs)))
	le.PutUint16(file[62:], 8) // e_shstrndx

	// Program headers.
	ph := func(idx int, off uint64, flags uint32, vaddr, paddr, filesz, memsz, align uint64) {
		b := file[0x40+idx*56 : 0x40+idx*56+56]
		le.PutUint32(b[0:], 1 /* PT_LOAD */)
		le.PutUint32(b[4:], flags)
		le.PutUint64(b[8:], off)
		le.PutUint64(b[16:], vaddr)
		le.PutUint64(b[24:], paddr)
		le.PutUint64(b[32:], filesz)
		le.PutUint64(b[40:], memsz)
		le.PutUint64(b[48:], align)
	}
	ph(0, 0x1000, 5 /* R|X */, 0x1000, 0x1000, 0x100, 0x100, 0x1000)
	ph(1, 0x2000, 6 /* R|W */, 0x3000, 0x3000, 0x500, 0x600, 0x1000)
	{
		b := file[0x40+112 : 0x40+112+56]
		le.PutUint32(b[0:], 0x6474e552) // PT_GNU_RELRO
		le.PutUint32(b[4:], 4)
		le.PutUint64(b[8:], 0x2000)
		le.PutUint64(b[16:], 0x3000)
		le.PutUint64(b[24:], 0x3000)
		le.PutUint64(b[32:], 0x300)
		le.PutUint64(b[40:], 0x300)
		le.PutUint64(b[48:], 0x1000)
	}

	copy(file[0x1000:0x1100], bytes.Repeat([]byte{0xc3}, 0x100)) // .text: RETs
	copy(file[0x2000:0x2000+len(data)], data)                    // .data
	copy(file[0x2800:0x2800+len(dynBuf)], dynBuf)
	copy(file[0x3000:0x3000+len(relaDyn)], relaDyn)
	copy(file[0x3200:0x3200+len(relaPlt)], relaPlt)
	copy(file[0x3400:0x3400+len(dynsym)], dynsym)
	copy(file[0x3600:0x3600+len(dynstr)], dynstr)
	copy(file[0x3800:0x3800+len(shstr)], shstr)

	for i, s := range shdrs {
		b := file[shoff+uint64(i)*64 : shoff+uint64(i)*64+64]
		le.PutUint32(b[0:], s.name)
		le.PutUint32(b[4:], s.typ)
		le.PutUint64(b[8:], s.flags)
		le.PutUint64(b[16:], s.addr)
		le.PutUint64(b[24:], s.off)
		le.PutUint64(b[32:], s.size)
		le.PutUint32(b[40:], s.link)
		le.PutUint32(b[44:], s.info)
		le.PutUint64(b[48:], s.align)
		le.PutUint64(b[56:], s.entsize)
	}

	path := filepath.Join(t.TempDir(), "libfixture.so")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// memAt reads a uintptr from mapped guest memory.
func memAt(im *Image, vaddr uint64) uintptr {
	return *(*uintptr)(unsafe.Pointer(im.Rebase(uintptr(vaddr))))
}

func TestLoadSyntheticFixture(t *testing.T) {
	path := buildFixture(t, fixtureOpts{})
	b, err := engine.Open(path)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	b.ClassifyImports()
	im, err := Load(b, Options{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	strlenAddr, ok := hostabi.Lookup("strlen")
	if !ok {
		t.Fatal("hostabi cannot provide strlen")
	}
	guardAddr, ok := hostabi.DataSymbol("__stack_chk_guard")
	if !ok {
		t.Fatal("hostabi cannot provide __stack_chk_guard")
	}

	// RELATIVE
	if got := memAt(im, 0x3080); got != im.Bias+0x1234 {
		t.Fatalf("RELATIVE: got %#x want %#x", got, im.Bias+0x1234)
	}
	// GLOB_DAT host_strlen
	if got := memAt(im, 0x3088); got != strlenAddr {
		t.Fatalf("GLOB_DAT strlen: got %#x want %#x", got, strlenAddr)
	}
	// GLOB_DAT data object
	if got := memAt(im, 0x3090); got != guardAddr {
		t.Fatalf("GLOB_DAT guard: got %#x want %#x", got, guardAddr)
	}
	// ABS64 defined function
	if got := memAt(im, 0x3098); got != im.Bias+0x1000 {
		t.Fatalf("ABS64: got %#x want %#x", got, im.Bias+0x1000)
	}
	// JUMP_SLOT
	if got := memAt(im, 0x30a0); got != strlenAddr {
		t.Fatalf("JUMP_SLOT: got %#x want %#x", got, strlenAddr)
	}

	// Exports
	v, ok := im.Exports["guest_func"]
	if !ok || v != im.Bias+0x1000 {
		t.Fatalf("exports guest_func: %#x ok=%v", v, ok)
	}

	// Init functions (DT_INIT only in this fixture)
	if len(im.InitFuncs) != 1 || im.InitFuncs[0] != im.Bias+0x1050 {
		t.Fatalf("InitFuncs: %#x", im.InitFuncs)
	}

	// Everything resolved: no trap thunks.
	if len(im.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved: %+v", im.Unresolved)
	}

	// RELRO has been applied: GOT slots are now read-only.
	// (Verified implicitly by not writing; an explicit mprotect check would
	// require disabling the mapping, so we check the reported range instead.)
	if im.relroStart != uintptr(alignUp(uint64(im.Bias)+0x3000, pageSize)) {
		t.Fatalf("relroStart: %#x", im.relroStart)
	}

	if err := im.RunInit(); err != nil {
		t.Fatalf("RunInit (only DT_INIT at 0x1050, zero bytes): %v", err)
	}
}

func TestLoadRejectsUnresolvedData(t *testing.T) {
	path := buildFixture(t, fixtureOpts{unresObj: true})
	b, err := engine.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b.ClassifyImports()
	if _, err := Load(b, Options{}); err == nil {
		t.Fatal("expected Load to fail on an unresolved strong OBJECT symbol")
	}
}
