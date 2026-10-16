package main

import (
	"fmt"
	"os"
	"path/filepath"

	"rbxrt/internal/apk"
	"rbxrt/internal/engine"
	"rbxrt/internal/loader"
)

// cmdRun launches a Roblox instance from the given instance directory.
//
//	rbxrt run <instance-dir>       launch a Roblox instance
func cmdRun(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rbxrt run <instance-dir>")
	}
	instanceDir := args[0]

	// Find the base.apk in the instance directory
	apkPath := filepath.Join(instanceDir, "base.apk")
	if _, err := os.Stat(apkPath); err != nil {
		return fmt.Errorf("base.apk not found in instance directory: %v", err)
	}

	// Verify the APK signature and extract it if needed
	fmt.Printf("Verifying APK: %s\n", apkPath)
	trusted, err := apk.TrustedCertificates()
	if err != nil {
		return err
	}
	v, err := apk.Verify(apkPath, trusted)
	if err != nil {
		return err
	}
	fmt.Printf("APK verified: scheme v%d, cert %s\n", v.Scheme, v.CertSHA256)

	// Extract APK if not already done
	extractDir := filepath.Join(instanceDir, "extracted")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return fmt.Errorf("failed to create extract directory: %v", err)
	}

	// Open the APK for extraction
	apkArchive, err := apk.OpenArchive(apkPath)
	if err != nil {
		return fmt.Errorf("failed to open APK: %v", err)
	}
	defer apkArchive.Close()

	// Extract all files from the APK
	fmt.Printf("Extracting APK to: %s\n", extractDir)
	for _, entry := range apkArchive.Entries() {
		dstPath := filepath.Join(extractDir, entry)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %v", entry, err)
		}
		if err := apkArchive.Extract(entry, dstPath); err != nil {
			return fmt.Errorf("failed to extract %s: %v", entry, err)
		}
	}
	fmt.Printf("APK extraction complete\n")

	// Find libroblox.so in the extracted APK. This runtime is a native x86-64
	// host, so it can only execute the x86_64 slice of a fat APK.
	libPath := filepath.Join(extractDir, "lib", "x86_64", "libroblox.so")
	if _, err := os.Stat(libPath); err != nil {
		return fmt.Errorf("lib/x86_64/libroblox.so not found in extracted APK: %v", err)
	}
	fmt.Printf("Found libroblox.so at: %s\n", libPath)

	// Load the library using our existing loader
	fmt.Printf("Loading libroblox.so...\n")
	b, err := engine.Open(libPath)
	if err != nil {
		return err
	}
	defer b.Close()

	b.ClassifyImports()

	loader.InstallHooks()
	im, err := loader.Load(b, loader.Options{})
	if err != nil {
		return err
	}

	fmt.Printf("Library loaded successfully\n")
	fmt.Printf("  Bias: 0x%x\n", im.Bias)
	fmt.Printf("  Base: 0x%x\n", im.Base)
	fmt.Printf("  Size: %d bytes\n", im.Size)
	fmt.Printf("  Exports: %d\n", len(im.Exports))
	fmt.Printf("  Init funcs: %d\n", len(im.InitFuncs))

	// Run constructors/init functions
	if err := im.RunInit(); err != nil {
		return err
	}
	fmt.Printf("Constructors executed: %d functions\n", len(im.InitFuncs))

	// TODO: Actually start the Roblox runtime/JNI environment
	// This would require:
	// 1. Setting up JVM/JNI environment
	// 2. Loading Roblox's Java classes from the APK
	// 3. Calling the appropriate entry points
	// 4. Handling graphics/audio/input via host abstractions

	fmt.Printf("Roblox instance initialized (runtime startup not yet implemented)\n")
	return nil
}
