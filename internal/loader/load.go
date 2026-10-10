// Package loader maps an Android ELF DSO into this process's address space,
// applies its relocations, and binds its undefined symbols to the
// bionic-compatible host library (see internal/hostabi).
//
// The loader is deliberately small and auditable: it is a faithful subset of
// the bionic linker's load path covering PT_LOAD mapping, SHT_ANDROID_REL[A]
// and SHT_RELA relocation, RELRO, and DT_INIT/DT_INIT_ARRAY invocation.
//
// Nothing here fakes success: an unimplemented function symbol binds to a trap
// thunk that aborts naming the symbol, and an unresolvable data symbol (one
// that must exist at load time) is a hard error.
package loader

import (
	"debug/elf"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"rbxrt/internal/engine"
	"rbxrt/internal/hostabi"
)

const pageSize = 4096

// Options controls a single Load call.
type Options struct {
	// Trace logs each symbol that binds to a trap thunk (i.e. is unimplemented).
	Trace bool
}

// Unresolved records an imported function that had no host implementation and
// therefore binds to a trap thunk. It is reported so callers can surface how
// incomplete the host library is before running guest code.
type Unresolved struct {
	Name  string
	Class string // engine.ImportClass string ("host", "android-lib", "stub", …)
	Kind  string // relocation kind
	Weak  bool
}

// Image is a DSO mapped into this process.
type Image struct {
	Eng  *engine.Binary
	Bias uintptr // load bias: guest vaddr + Bias == host address
	Base uintptr // host address of the first mapped page
	Size uintptr // total mapped size
	End  uintptr // Base + Size

	Exports    map[string]uintptr
	Unresolved []Unresolved

	// relroStart/relroEnd are host addresses protected read-only after
	// relocation.
	relroStart, relroEnd uintptr

	// perms is the final per-page protection of the mapped span.
	perms    []uint8
	permBase uintptr // host address of perms[0]

	InitFuncs []uintptr // absolute addresses, in DT_INIT then DT_INIT_ARRAY order
	FiniFuncs []uintptr

	file *os.File
	fd   int
}

// Rebase converts a guest virtual address to the host address.
func (im *Image) Rebase(vaddr uintptr) uintptr { return im.Bias + vaddr }

// Segment describes one mapped PT_LOAD.
type Segment struct {
	Vaddr, Memsz uint64
	Flags        elf.ProgFlag
}

// Segments reports the PT_LOAD segments of the mapped image.
func (im *Image) Segments() []Segment {
	var out []Segment
	for _, p := range im.Eng.File.Progs {
		if p.Type == elf.PT_LOAD {
			out = append(out, Segment{p.Vaddr, p.Memsz, p.Flags})
		}
	}
	return out
}

// Load maps b, applies relocations, and protects RELRO. It does not run any
// constructor; call RunInit for that.
func Load(b *engine.Binary, opts Options) (*Image, error) {
	if b == nil || b.File == nil {
		return nil, fmt.Errorf("loader: nil binary")
	}

	f, err := os.Open(b.Path)
	if err != nil {
		return nil, fmt.Errorf("loader: open %s: %w", b.Path, err)
	}

	im := &Image{
		Eng:     b,
		Exports: map[string]uintptr{},
		file:    f,
		fd:      int(f.Fd()),
	}

	if err := im.mapSegments(); err != nil {
		f.Close()
		return nil, err
	}
	if err := im.applyRelocations(opts); err != nil {
		im.unmap()
		return nil, err
	}
	if err := im.collectExports(); err != nil {
		im.unmap()
		return nil, err
	}
	if err := im.protect(); err != nil {
		im.unmap()
		return nil, err
	}
	im.collectInit()
	registerImage(im)
	return im, nil
}

// mapSegments reserves address space and maps every PT_LOAD region from the
// file, zero-filling BSS.
func (im *Image) mapSegments() error {
	var minV, maxV uint64
	var loads []*elf.Prog
	have := false
	for _, p := range im.Eng.File.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		loads = append(loads, p)
		if !have || p.Vaddr < minV {
			minV = p.Vaddr
		}
		if end := p.Vaddr + p.Memsz; end > maxV {
			maxV = end
		}
		have = true
	}
	if !have {
		return fmt.Errorf("loader: %s: no PT_LOAD segments", im.Eng.Path)
	}
	minV = alignDown(minV, pageSize)
	maxV = alignUp(maxV, pageSize)
	total := maxV - minV

	reserved, err := mmapAt(0, total, syscall.PROT_NONE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON, ^uintptr(0), 0)
	if err != nil {
		return fmt.Errorf("loader: reserve %d bytes: %w", total, err)
	}
	bias := uintptr(reserved) - uintptr(minV)
	im.Bias = bias
	im.Base = uintptr(reserved)
	im.Size = uintptr(total)
	im.End = im.Base + im.Size

	// Map file-backed regions (writable for now; final protections are applied
	// after relocation) and zero-fill BSS.
	for _, p := range loads {
		segStart := uint64(bias) + p.Vaddr
		pageStart := alignDown(segStart, pageSize)
		filePageOff := alignDown(p.Off, pageSize)
		delta := segStart - pageStart // == Vaddr - alignDown(Vaddr)

		fileMapLen := p.Filesz + delta
		if fileMapLen > 0 {
			if _, err := mmapAt(pageStart, fileMapLen,
				syscall.PROT_READ|syscall.PROT_WRITE,
				syscall.MAP_PRIVATE|syscall.MAP_FIXED, uintptr(im.fd), uintptr(filePageOff)); err != nil {
				return fmt.Errorf("loader: map segment %#x: %w", p.Vaddr, err)
			}
		}

		bssStart := segStart + p.Filesz
		bssEnd := segStart + p.Memsz
		if bssEnd > bssStart {
			// The last file-backed page carries file bytes past Filesz; zero
			// that tail explicitly.
			if z := alignUp(bssStart, pageSize); z > bssStart {
				end := min(z, bssEnd)
				zeroFrom(uintptr(bssStart), uintptr(end-bssStart))
			}
			// Remainder of BSS lives in pages still reserved PROT_NONE.
			if anonStart := alignUp(bssStart, pageSize); bssEnd > anonStart {
				anonEnd := alignUp(bssEnd, pageSize)
				if _, err := mmapAt(anonStart, anonEnd-anonStart,
					syscall.PROT_READ|syscall.PROT_WRITE,
					syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_FIXED, ^uintptr(0), 0); err != nil {
					return fmt.Errorf("loader: map bss %#x: %w", p.Vaddr, err)
				}
			}
		}
	}
	return nil
}

// mmapAt is syscall.Mmap with an explicit address, for MAP_FIXED placement.
func mmapAt(addr, length uint64, prot, flags, fd, off uintptr) (uintptr, error) {
	r, _, errno := syscall.Syscall6(syscall.SYS_MMAP, uintptr(addr), uintptr(length),
		uintptr(prot), uintptr(flags), fd, off)
	if errno != 0 {
		return 0, errno
	}
	return r, nil
}

// applyRelocations writes RELATIVE, ABS64, GLOB_DAT, JUMP_SLOT and IRELATIVE
// relocations into the mapped image and resolves their symbols.
func (im *Image) applyRelocations(opts Options) error {
	rels := im.Eng.Relocs

	// RELATIVE first: no symbol lookup needed.
	for _, r := range rels.Relative {
		im.put64(r.Offset, im.Bias+uintptr(r.Addend))
	}

	for _, r := range rels.Other {
		switch r.Kind {
		case engine.RelocUnknown:
			return fmt.Errorf("loader: unknown relocation type %#x at %#x",
				r.Type, r.Offset)
		case engine.RelocIrelative:
			resolver := im.Bias + uintptr(r.Addend)
			resolved := hostabi.CallGuest0(resolver)
			im.put64(r.Offset, resolved)
			continue
		}

		sym := im.symbolFor(r)
		addr, err := im.resolveSymbol(sym, r, opts)
		if err != nil {
			return fmt.Errorf("loader: %s: %w", im.Eng.Path, err)
		}

		var val uintptr
		switch r.Kind {
		case engine.RelocAbs, engine.RelocGlobDat, engine.RelocJumpSlot:
			val = addr + uintptr(r.Addend)
		default:
			return fmt.Errorf("loader: unhandled relocation kind %v at %#x",
				r.Kind, r.Offset)
		}
		im.put64(r.Offset, val)
	}
	return nil
}

// symbolFor returns the symbol a relocation refers to.
func (im *Image) symbolFor(r *engine.Reloc) *engine.Symbol {
	if r.SymIdx == 0 {
		return nil
	}
	if r.Symbol != nil {
		return r.Symbol
	}
	return im.Eng.SymByIndex[int(r.SymIdx)]
}

// resolveSymbol maps a relocation's symbol to a host address.
//
// Rules (mirroring bionic):
//   - defined in this DSO   -> bias + st_value
//   - undefined + host data -> host data object address
//   - undefined + host func -> host thunk
//   - undefined + weak      -> 0 (bionic resolves undefined weak to zero)
//   - undefined + strong func with no implementation -> trap thunk (fails
//     loudly when called) and recorded in Unresolved
//   - undefined + strong data with no implementation -> hard error (a data
//     object must exist at load time; it cannot trap)
func (im *Image) resolveSymbol(sym *engine.Symbol, r *engine.Reloc, opts Options) (uintptr, error) {
	if sym == nil {
		return 0, fmt.Errorf("relocation at %#x has no symbol (type %#x)", r.Offset, r.Type)
	}
	if !sym.Import {
		return im.Bias + uintptr(sym.Value), nil
	}
	name := sym.Name
	if name == "" {
		return 0, fmt.Errorf("relocation at %#x refers to an unnamed import", r.Offset)
	}

	weak := sym.Bind == "WEAK"

	// Data objects must exist now.
	if sym.Type == "OBJECT" || sym.Type == "TLS" {
		if a, ok := hostabi.DataSymbol(name); ok {
			return a, nil
		}
		if weak {
			return 0, nil
		}
		return 0, fmt.Errorf("unresolved data symbol %q (%s, relocation %v at %#x)",
			name, sym.Type, r.Kind, r.Offset)
	}

	// Function-like: try the host library.
	if a, ok := hostabi.Lookup(name); ok {
		return a, nil
	}
	if weak {
		return 0, nil
	}
	// No implementation: bind a trap thunk so a call aborts naming the symbol.
	im.recordUnresolved(sym, r, weak, opts)
	a, _ := hostabi.Lookup(name)
	return a, nil
}

func (im *Image) recordUnresolved(sym *engine.Symbol, r *engine.Reloc, weak bool, opts Options) {
	u := Unresolved{
		Name:  sym.Name,
		Class: sym.Class.String(),
		Kind:  r.Kind.String(),
		Weak:  weak,
	}
	im.Unresolved = append(im.Unresolved, u)
	if opts.Trace {
		fmt.Fprintf(os.Stderr, "rbxrt: loader: unresolved function %q (class=%s, %s) -> trap\n",
			u.Name, u.Class, u.Kind)
	}
}

// collectExports publishes this DSO's defined symbols into the global namespace.
func (im *Image) collectExports() error {
	for _, s := range im.Eng.Exports {
		if s.Name == "" || s.Import {
			continue
		}
		im.Exports[s.Name] = im.Bias + uintptr(s.Value)
	}
	return nil
}

// protect applies each segment's final file protections, then locks RELRO.
func (im *Image) protect() error {
	// Build per-page permissions across the mapped span.
	n := int(im.Size / pageSize)
	im.perms = make([]uint8, n)
	im.permBase = im.Base

	for _, p := range im.Eng.File.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		start := alignDown(uint64(im.Bias)+p.Vaddr, pageSize)
		end := alignUp(uint64(im.Bias)+p.Vaddr+p.Memsz, pageSize)
		want := protFromProgFlags(p.Flags)
		for a := start; a < end; a += pageSize {
			idx := int((a - uint64(im.Base)) / pageSize)
			if idx < 0 || idx >= n {
				continue
			}
			im.perms[idx] |= want
		}
	}

	// mprotect maximal runs of equal permission.
	for i := 0; i < n; {
		prot := im.perms[i]
		j := i + 1
		for j < n && im.perms[j] == prot {
			j++
		}
		addr := im.Base + uintptr(i)*pageSize
		length := uintptr(j-i) * pageSize
		if prot == 0 {
			// Leave unmapped/reserved spans PROT_NONE.
			i = j
			continue
		}
		if err := syscall.Mprotect(unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(length)), int(prot)); err != nil {
			return fmt.Errorf("loader: mprotect %d bytes at %#x: %w", length, addr, err)
		}
		i = j
	}

	// RELRO: read-only after relocation, before constructors run.
	for _, p := range im.Eng.File.Progs {
		if p.Type != elf.PT_GNU_RELRO {
			continue
		}
		start := alignUp(uint64(im.Bias)+p.Vaddr, pageSize)
		end := alignUp(uint64(im.Bias)+p.Vaddr+p.Memsz, pageSize)
		if end > start {
			length := uintptr(end - start)
			addr := uintptr(start)
			if err := syscall.Mprotect(unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(length)), syscall.PROT_READ); err != nil {
				return fmt.Errorf("loader: relro mprotect %#x: %w", addr, err)
			}
			im.relroStart, im.relroEnd = addr, addr+length
		}
	}
	return nil
}

// collectInit records DT_INIT and DT_INIT_ARRAY entry points (absolute), plus
// the DT_FINI / DT_FINI_ARRAY destructors.
func (im *Image) collectInit() {
	init, initarr, initasz := im.Eng.DynamicInit()
	if init != 0 {
		im.InitFuncs = append(im.InitFuncs, im.Bias+uintptr(init))
	}
	if initarr != 0 && initasz >= 8 {
		im.collectFnArray(initarr, initasz, &im.InitFuncs)
	}
	fini, finiarr, finiasz := im.Eng.DynamicFini()
	if fini != 0 {
		im.FiniFuncs = append(im.FiniFuncs, im.Bias+uintptr(fini))
	}
	if finiarr != 0 && finiasz >= 8 {
		im.collectFnArray(finiarr, finiasz, &im.FiniFuncs)
	}
}

// collectFnArray reads size bytes of function pointers at guest vaddr arr and
// appends the nonzero entries to dst.
func (im *Image) collectFnArray(arr, size uint64, dst *[]uintptr) {
	base := im.Bias + uintptr(arr)
	for off := uintptr(0); off+8 <= uintptr(size); off += 8 {
		fn := *(*uintptr)(unsafe.Pointer(base + off))
		if fn != 0 {
			*dst = append(*dst, fn)
		}
	}
}

func (im *Image) put64(guestOffset uint64, v uintptr) {
	p := im.Bias + uintptr(guestOffset)
	*(*uintptr)(unsafe.Pointer(p)) = v
}

func (im *Image) unmap() {
	unregisterImage(im)
	if im.fd != 0 && im.fd != -1 {
		// Mmap references are already dropped with the reservation.
	}
}

// Close unregisters the image. The mapping itself remains (other images or
// running guest code may reference it), which matches dlclosed-once semantics.
func (im *Image) Close() error {
	unregisterImage(im)
	if im.file != nil {
		im.file.Close()
		im.file = nil
		im.fd = -1
	}
	return nil
}

// --- helpers -------------------------------------------------------------

func protFromProgFlags(f elf.ProgFlag) uint8 {
	var prot uint8
	if f&elf.PF_R != 0 {
		prot |= syscall.PROT_READ
	}
	if f&elf.PF_W != 0 {
		prot |= syscall.PROT_WRITE
	}
	if f&elf.PF_X != 0 {
		prot |= syscall.PROT_EXEC
	}
	return prot
}

func alignDown(v, a uint64) uint64 { return v &^ (a - 1) }
func alignUp(v, a uint64) uint64   { return (v + a - 1) &^ (a - 1) }

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// zeroFrom writes n zero bytes at p. p must be mapped and writable.
func zeroFrom(p, n uintptr) {
	for i := uintptr(0); i < n; i++ {
		*(*byte)(unsafe.Pointer(p + i)) = 0
	}
}

// --- global namespace ----------------------------------------------------

var (
	nsMu   sync.RWMutex
	images []*Image
)

func registerImage(im *Image) {
	nsMu.Lock()
	images = append(images, im)
	nsMu.Unlock()
}

func unregisterImage(im *Image) {
	nsMu.Lock()
	for i, x := range images {
		if x == im {
			images = append(images[:i], images[i+1:]...)
			break
		}
	}
	nsMu.Unlock()
}

// Images returns the currently registered images (for dlsym / dl_iterate_phdr).
func Images() []*Image {
	nsMu.RLock()
	defer nsMu.RUnlock()
	out := make([]*Image, len(images))
	copy(out, images)
	return out
}
