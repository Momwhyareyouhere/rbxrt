package main

import (
	"fmt"
	"os"

	"rbxrt/internal/engine"
)

func cmdEngine(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rbxrt engine <lib.so> [import <name> [name...]]")
	}
	b, err := engine.Open(args[0])
	if err != nil {
		return err
	}
	defer b.Close()

	b.Summary(os.Stdout)

	fmt.Printf("  relocs:    relative %d, other %d (total %d)\n",
		len(b.Relocs.Relative), len(b.Relocs.Other), b.Relocs.Total)
	jumps := 0
	jiff := 0
	for _, r := range b.Relocs.Other {
		if r.Kind == engine.RelocJumpSlot {
			jumps++
		}
		if r.Kind == engine.RelocIrelative {
			jiff++
		}
	}
	fmt.Printf("             (= %d JUMP_SLOT, %d IRELATIVE, %d other non-relative)\n",
		jumps, jiff, len(b.Relocs.Other)-jumps-jiff)

	// Summarize relocation types by raw type number.
	typeCount := map[uint64]int{}
	for _, r := range b.Relocs.Relative {
		typeCount[r.Type]++
	}
	for _, r := range b.Relocs.Other {
		typeCount[r.Type]++
	}
	// top 8 type numbers
	fmt.Printf("  reloc types:\n")
	shown := 0
	for t := uint64(0); t < 64; t++ {
		if n, ok := typeCount[t]; ok && shown < 8 {
			fmt.Printf("    type %d -> %d\n", t, n)
			shown++
		}
	}

	// JNI and notable exports.
	fmt.Printf("  exports:\n")
	notable := 0
	for _, e := range b.Exports {
		if e.Name == "JNI_OnLoad" || e.Name == "JNI_OnUnload" ||
			(len(e.Name) > 4 && e.Name[:4] == "Java") {
			fmt.Printf("    %s%s (%s)\n", e.Name, versionSuffix(e), e.Type)
			notable++
		}
	}
	if notable == 0 {
		fmt.Printf("    (none matching JNI_*)")
	}

	// Classification.
	b.ClassifyImports()
	rep := b.ReportClassification()
	fmt.Printf("  imports:\n")
	rep.PrintClassification(os.Stdout)

	// Specific import detail.
	if len(args) > 1 && args[1] == "import" {
		for _, name := range args[2:] {
			found := false
			for _, s := range b.Imports {
				if s.Name == name {
					found = true
					fmt.Printf("  %s -> provider %q class %s%s\n",
						s.Name, s.Provider, s.Class, versionSuffix(s))
				}
			}
			if !found {
				fmt.Printf("  %s -> (not imported)\n", name)
			}
		}
	}
	return nil
}

func versionSuffix(s *engine.Symbol) string {
	if s.Version != "" {
		return "@" + s.Version
	}
	return ""
}
