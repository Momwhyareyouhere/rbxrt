package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"rbxrt/internal/apk"
	"rbxrt/internal/fetch"
)

// An instance directory holds everything one Roblox installation needs:
//
//	<instance>/base.apk                   the client, verified before every use
//	<instance>/lib/x86_64/libroblox.so    the engine, extracted from base.apk
//	<instance>/data/                      app data: assets, caches, logs
//
// rbxrt fetch fills base.apk; rbxrt run fetches it on first use.

func instanceAPK(dir string) string  { return filepath.Join(dir, "base.apk") }
func instanceLib(dir string) string  { return filepath.Join(dir, "lib", "x86_64", "libroblox.so") }
func instanceData(dir string) string { return filepath.Join(dir, "data") }

// cmdFetch downloads the latest Roblox APK into an instance directory.
//
//	rbxrt fetch <instance-dir>
func cmdFetch(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rbxrt fetch <instance-dir>")
	}
	_, err := fetchInto(args[0])
	return err
}

func fetchInto(dir string) (*fetch.Result, error) {
	dest := instanceAPK(dir)
	fmt.Printf("fetch: downloading the latest %s from the mirror (%s)\n", fetch.Package, fetch.MirrorURL(fetch.Package))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r, err := fetch.Download(ctx, dest, fetch.Options{Progress: progressPrinter(os.Stderr)})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	fmt.Printf("fetch: %s %s (code %d), %d MB\n", fetch.Package, r.VersionName, r.VersionCode, r.Size>>20)
	fmt.Printf("fetch: signature v%d OK, certificate %s (%s)\n", r.Verification.Scheme, r.Verification.CertSHA256, r.Verification.Subject)
	fmt.Printf("fetch: sha256 %s\n", r.SHA256)
	fmt.Printf("fetch: saved %s\n", r.Path)
	return r, nil
}

func progressPrinter(w io.Writer) func(done, total int64) {
	return func(done, total int64) {
		if total > 0 {
			fmt.Fprintf(w, "\rfetch: %d / %d MB (%d%%)", done>>20, total>>20, done*100/total)
		} else {
			fmt.Fprintf(w, "\rfetch: %d MB", done>>20)
		}
	}
}

// cmdRun launches a Roblox instance: it fetches the client on first use,
// verifies it, extracts the x86_64 engine, and boots it in a window until it
// is stopped (Ctrl-C).
//
//	rbxrt run <instance-dir> [--update]
func cmdRun(args []string) error {
	var dir string
	update := false
	for _, a := range args {
		switch {
		case a == "--update":
			update = true
		case dir == "" && a != "" && a[0] != '-':
			dir = a
		default:
			return fmt.Errorf("usage: rbxrt run <instance-dir> [--update]")
		}
	}
	if dir == "" {
		return fmt.Errorf("usage: rbxrt run <instance-dir> [--update]")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	apkPath := instanceAPK(dir)
	if _, err := os.Stat(apkPath); os.IsNotExist(err) || update {
		if !update {
			fmt.Printf("run: no client in %s yet\n", dir)
		}
		if _, err := fetchInto(dir); err != nil {
			return err
		}
	}

	// Verify on every launch: the file on disk is what runs.
	trusted, err := apk.TrustedCertificates()
	if err != nil {
		return err
	}
	v, err := apk.Verify(apkPath, trusted)
	if err != nil {
		return fmt.Errorf("%s: %w", apkPath, err)
	}
	fmt.Printf("run: %s verified (signature v%d, Roblox certificate)\n", apkPath, v.Scheme)

	lib, err := extractEngine(apkPath, instanceLib(dir))
	if err != nil {
		return err
	}

	// The launch reads its inputs from the environment (see the README's
	// boot inputs); an explicit setting by the user wins.
	setDefault := func(k, v string) {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	setDefault("RBXRT_APK", apkPath)
	setDefault("RBXRT_DATA", instanceData(dir))
	setDefault("RBXRT_BOOT", "1")
	if os.Getenv("RBXRT_LAUNCH_HOLD_SECS") == "" {
		holdForever = true
	}
	// Quit on Ctrl-C / SIGTERM. Relying on Go's default disposition is not
	// enough once engine threads run: the signal can land on a thread that
	// is executing guest code, and the default exit never happens. A
	// subscribed signal is delivered to this goroutine instead.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-quit
		fmt.Printf("\nrun: %v received, quitting\n", s)
		code := 130 // 128 + SIGINT
		if s == syscall.SIGTERM {
			code = 143
		}
		os.Exit(code)
	}()
	return cmdLoad([]string{lib, "--launch"})
}

// extractEngine writes lib/x86_64/libroblox.so from apkPath to dst, unless dst
// is already newer than the APK. The file is written beside dst and renamed,
// so an interrupted extraction never leaves a partial library behind.
func extractEngine(apkPath, dst string) (string, error) {
	ai, err := os.Stat(apkPath)
	if err != nil {
		return "", err
	}
	if li, err := os.Stat(dst); err == nil && li.ModTime().After(ai.ModTime()) && li.Size() > 0 {
		return dst, nil
	}
	a, err := apk.OpenArchive(apkPath)
	if err != nil {
		return "", err
	}
	defer a.Close()
	const entry = "lib/x86_64/libroblox.so"
	if !a.Has(entry) {
		return "", fmt.Errorf("%s has no %s: rbxrt needs the x86_64 (universal) APK", apkPath, entry)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".partial"
	if err := a.Extract(entry, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	fmt.Printf("run: extracted %s\n", dst)
	return dst, nil
}
