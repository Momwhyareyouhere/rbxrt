package main

import (
	"fmt"
	"os"
	"strconv"

	"rbxrt/internal/engine"
)

// debugreloc prints the dynamic symbols referenced by relocations at the given
// r_offsets. Throwaway tool used while bringing the engine up.
func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: debugreloc <so> <offset-hex> [offset-hex...]")
		os.Exit(2)
	}
	b, err := engine.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer b.Close()
	b.ClassifyImports()

	want := map[uint64]bool{}
	for _, a := range os.Args[2:] {
		v, err := strconv.ParseUint(a, 0, 64)
		if err != nil {
			panic(err)
		}
		want[v] = true
	}

	report := func(r *engine.Reloc) {
		if !want[r.Offset] {
			return
		}
		name := "<no symbol>"
		if r.Symbol != nil {
			name = r.Symbol.Name
		} else if r.SymIdx != 0 {
			if s, ok := b.SymByIndex[int(r.SymIdx)]; ok {
				name = s.Name
			}
		}
		fmt.Printf("offset=%#x kind=%s type=%d sym=%d symname=%s addend=%d grouped=%v\n",
			r.Offset, r.Kind, r.Type, r.SymIdx, name, r.Addend, r.Grouped)
	}
	for _, r := range b.Relocs.Relative {
		report(r)
	}
	for _, r := range b.Relocs.Other {
		report(r)
	}
	fmt.Printf("total relocs: %d\n", b.Relocs.Total)
}
