package main

import (
	"fmt"

	"rbxrt/internal/apk"
)

func cmdManifest(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rbxrt manifest <apk>")
	}
	a, err := apk.OpenArchive(args[0])
	if err != nil {
		return err
	}
	defer a.Close()
	data, err := a.Read("AndroidManifest.xml")
	if err != nil {
		return err
	}
	m, err := apk.ParseManifest(data)
	if err != nil {
		return err
	}
	fmt.Printf("package:      %s\n", m.Package)
	fmt.Printf("versionCode:  %d\n", m.VersionCode)
	fmt.Printf("versionName:  %s\n", m.VersionName)
	fmt.Printf("minSdk:       %d\n", m.MinSDK)
	fmt.Printf("targetSdk:    %d\n", m.TargetSDK)
	for _, e := range m.Elements {
		fmt.Printf("<%s>\n", e.Name)
		for _, at := range e.Attrs {
			ns := at.NS
			if ns == "" {
				ns = "-"
			}
			fmt.Printf("  %s:%s = %q (res 0x%08x, type 0x%02x, int %d)\n",
				ns, at.Name, at.Value, at.ResID, at.Type, at.Int)
		}
	}
	return nil
}
