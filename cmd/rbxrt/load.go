package main

import (
	"debug/elf"
	"fmt"
	"strings"

	"rbxrt/internal/engine"
	"rbxrt/internal/loader"
)

// cmdLoad loads an ELF shared object into this process like the bionic linker
// would (map segments, apply relocations, bind undefined symbols), reporting
// what resolved and what fell back to trap thunks.
//
//	rbxrt load <lib.so> [--init] [--trace]
func cmdLoad(args []string) error {
	var (
		doInit  bool
		tracing bool
		path    string
	)
	for _, a := range args {
		switch {
		case a == "--init":
			doInit = true
		case a == "--trace":
			tracing = true
		case strings.HasPrefix(a, "-") && a != "-":
			return fmt.Errorf("unknown flag %s", a)
		case path == "":
			path = a
		default:
			return fmt.Errorf("unexpected argument %s", a)
		}
	}
	if path == "" {
		return fmt.Errorf("usage: rbxrt load <lib.so> [--init] [--trace]")
	}

	b, err := engine.Open(path)
	if err != nil {
		return err
	}
	b.ClassifyImports()

	loader.InstallHooks()
	im, err := loader.Load(b, loader.Options{Trace: tracing})
	if err != nil {
		return err
	}

	fmt.Printf("loaded:   %s\n", path)
	fmt.Printf("soname:   %s\n", orDash(b.SOName))
	fmt.Printf("bias:     0x%x\n", im.Bias)
	fmt.Printf("base:     0x%x\n", im.Base)
	fmt.Printf("span:     %d bytes (0x%x..0x%x)\n", im.Size, im.Base, im.End)
	for _, s := range im.Segments() {
		fmt.Printf("segment:  vaddr=0x%x memsz=0x%x flags=%s\n",
			s.Vaddr, s.Memsz, segmentFlags(s.Flags))
	}
	fmt.Printf("exports:  %d\n", len(im.Exports))
	fmt.Printf("inits:    %d (DT_INIT + DT_INIT_ARRAY)\n", len(im.InitFuncs))
	fmt.Printf("finis:    %d\n", len(im.FiniFuncs))

	if len(im.Unresolved) > 0 {
		fmt.Printf("unresolved: %d (bound to trap thunks that abort when called)\n",
			len(im.Unresolved))
		for _, u := range im.Unresolved {
			fmt.Printf("  ! %s (class=%s, kind=%s%s)\n",
				u.Name, u.Class, u.Kind, weakNote(u.Weak))
		}
	} else {
		fmt.Printf("unresolved: 0 (every strong function provided; no traps)\n")
	}

	if doInit {
		if err := im.RunInit(); err != nil {
			return err
		}
		fmt.Printf("constructors: ran %d functions\n", len(im.InitFuncs))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func weakNote(weak bool) string {
	if weak {
		return ", weak"
	}
	return ""
}

func segmentFlags(f elf.ProgFlag) string {
	var sb strings.Builder
	if f&elf.PF_X != 0 {
		sb.WriteString("X")
	}
	if f&elf.PF_W != 0 {
		sb.WriteString("W")
	}
	if f&elf.PF_R != 0 {
		sb.WriteString("R")
	}
	return sb.String()
}
