package main

import (
	"debug/elf"
	"fmt"
	"os"
	"strings"

	"rbxrt/internal/engine"
	"rbxrt/internal/hostabi"
	"rbxrt/internal/loader"
)

// cmdLoad loads an ELF shared object into this process like the bionic linker
// would (map segments, apply relocations, bind undefined symbols), reporting
// what resolved and what fell back to trap thunks.
//
//	rbxrt load <lib.so> [--init] [--trace] [--jni] [--entry] [--launch]
func cmdLoad(args []string) error {
	var (
		doInit  bool
		tracing bool
		jni     bool
		entry   bool
		launch  bool
		gfx     bool
		path    string
	)
	for _, a := range args {
		switch {
		case a == "--init":
			doInit = true
		case a == "--trace":
			tracing = true
		case a == "--jni":
			jni = true
		case a == "--entry":
			// Driving the app entry point only makes sense once the library is
			// initialized, so --entry implies --init and --jni.
			entry = true
			doInit = true
			jni = true
		case a == "--gfx":
			gfx = true
		case a == "--launch":
			// --launch drives the entry point and then plays the Java
			// GameActivity lifecycle around it.
			launch = true
			entry = true
			doInit = true
			jni = true
		case strings.HasPrefix(a, "-") && a != "-":
			return fmt.Errorf("unknown flag %s", a)
		case path == "":
			path = a
		default:
			return fmt.Errorf("unexpected argument %s", a)
		}
	}
	if path == "" {
		return fmt.Errorf("usage: rbxrt load <lib.so> [--init] [--trace] [--jni] [--entry] [--launch] [--gfx]")
	}

	// Graphics is on for --launch (there is nothing to render without it), off
	// if RBXRT_GFX=off, and forced on by RBXRT_GFX=1.
	envGfxOn, envGfxOff := gfxEnv()
	if envGfxOn {
		gfx = true
	}
	if launch && !envGfxOff {
		gfx = true
	}

	b, err := engine.Open(path)
	if err != nil {
		return err
	}
	b.ClassifyImports()

	loader.InstallHooks()
	// Bring up the graphics platform *before* loading, so the engine's gl*/egl*
	// imports bind to the host's real Mesa implementation rather than trap
	// thunks. A failure here is not fatal: the engine will still abort, loudly,
	// at the first graphics call.
	if gfx {
		if err := hostabi.InitGraphics(); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: graphics: %v\n", err)
			fmt.Fprintf(os.Stderr, "rbxrt: graphics: gl*/egl* calls will abort by name\n")
		} else {
			w, h := hostabi.WindowSize()
			fmt.Printf("graphics: X11 window bound to libEGL/libGLESv2 (%dx%d), Mesa will render\n", w, h)
		}
	}
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

	// Guest code is about to run (constructors and/or JNI_OnLoad). Apply the
	// telemetry stop-gap first: the constructors start worker threads, and those
	// threads must see the patch too. See applyTelemetryWorkaround.
	if doInit || jni {
		applyTelemetryWorkaround(im.Bias)
	}

	if doInit {
		if debugMem() {
			dumpMapsNear(im.Bias)
			dumpArena()
		}
		if err := im.RunInit(); err != nil {
			return err
		}
		fmt.Printf("constructors: ran %d functions\n", len(im.InitFuncs))
	}

	if jni {
		if err := callJNIOnLoad(im); err != nil {
			return err
		}
	}
	if entry {
		h, err := probeInitializeNativeCode(im)
		if err != nil {
			return err
		}
		if launch {
			return launchGameActivity(h)
		}
	}
	return nil
}

// dumpArena prints the host code arena's extent and the bytes around the
// offsets guest control keeps landing in (see the JNI crash notes), plus the
// trap-thunk ledger so a jump into the arena can be attributed to a symbol.
func dumpArena() {
	base, used, size := hostabi.ArenaRange()
	fmt.Printf("arena:   base=0x%x used=0x%x size=0x%x\n", base, used, size)
	if base == 0 {
		return
	}
	// The region of interest: 0x2400..0x2c00 (the crash pcs resolve to
	// arena+0x27aa and arena+0x298a).
	b := hostabi.ArenaDump()
	lo, hi := 0x2400, 0x2c00
	if hi > len(b) {
		hi = len(b)
	}
	for o := lo; o < hi; o += 16 {
		var chars [16]byte
		for i := 0; i < 16 && o+i < hi; i++ {
			c := b[o+i]
			if c < 0x20 || c > 0x7e {
				chars[i] = '.'
			} else {
				chars[i] = c
			}
		}
		fmt.Printf("arena+0x%04x: % x  %s\n", o, b[o:o+16], string(chars[:]))
	}
	tm := hostabi.DebugTrapMap()
	fmt.Printf("traps:   %d trap thunks\n", len(tm))
	for n, a := range tm {
		if a >= base+0x2000 && a < base+0x4000 {
			fmt.Printf("  trap %-40s = arena+0x%04x\n", n, a-base)
		}
	}
}

// callJNIOnLoad drives the guest's JNI_OnLoad against the host JNI environment
// and reports what it does. The telemetry stop-gap is applied by cmdLoad before
// any guest code runs, so the constructors' worker threads see it too; see
// applyTelemetryWorkaround.
func callJNIOnLoad(im *loader.Image) error {
	vm := hostabi.JavaVM()
	jniOnLoad, ok := im.Exports["JNI_OnLoad"]
	if !ok {
		return fmt.Errorf("no JNI_OnLoad export in this image")
	}
	fmt.Printf("calling JNI_OnLoad (vm=%#x, bias=%#x)\n", vm, im.Bias)
	rv := hostabi.CallGuestInt3(jniOnLoad, vm, 0, 0)
	fmt.Printf("JNI_OnLoad returned %#x (%d)\n", rv, rv)
	return nil
}

// entryPoint is the native method the Java GameActivity calls once its view and
// asset manager exist. Its prototype was recovered from the APK's classes2.dex:
//
//	initializeNativeCode(String assetPath, String nativeLibDir,
//	                     String internalDataDir, AssetManager assets,
//	                     byte[] something, Configuration config) -> long
//
// which, once env and thiz are prepended, is eight integer arguments: six in
// registers and two on the stack. It is the real app entry point — JNI_OnLoad
// only initializes the library.
const entryPoint = "Java_com_google_androidgamesdk_GameActivity_initializeNativeCode"

// probeInitializeNativeCode drives the app entry point with a fabricated JNI
// environment so we can measure how far it gets and which platform import stops
// it first. Nothing here fakes success: every argument is a distinct non-null
// sentinel, our JNI table answers the calls it receives, and the probe is
// expected to abort — loudly — at the first unimplemented platform function
// (the body's first external calls are __android_log_print, then
// ALooper_forThread, pipe, fcntl and AAssetManager_fromJava).
func probeInitializeNativeCode(im *loader.Image) (uintptr, error) {
	fn, ok := im.Exports[entryPoint]
	if !ok {
		return 0, fmt.Errorf("no %s export in this image", entryPoint)
	}

	// Fabricated Java references. The engine treats these as opaque jobjects
	// (compares them, hands them back to the JNI table) until it reaches a
	// platform import that would actually dereference them, at which point we
	// want the trap to fire rather than a silent zero.
	env := hostabi.JNIEnvAddr()
	const (
		thiz         = uintptr(0x7b010000) // GameActivity instance
		assetPath    = uintptr(0x7b010001) // String: path to the APK/assets
		nativeLibDir = uintptr(0x7b010002) // String: native library directory
		internalDir  = uintptr(0x7b010003) // String: internal data directory
		assetManager = uintptr(0x7b010004) // android.content.res.AssetManager
		insets       = uintptr(0x7b010005) // byte[]
		config       = uintptr(0x7b010006) // android.content.res.Configuration
	)

	fmt.Printf("probing %s (fn=%#x env=%#x)\n", entryPoint, fn, env)
	fmt.Printf("  args: thiz=%#x assetPath=%#x nativeLibDir=%#x internalDir=%#x assets=%#x blob=%#x config=%#x\n",
		thiz, assetPath, nativeLibDir, internalDir, assetManager, insets, config)
	rv := hostabi.CallGuestInt8(fn, env, thiz, assetPath, nativeLibDir, internalDir,
		assetManager, insets, config)
	fmt.Printf("initializeNativeCode returned %#x (%d)\n", rv, rv)
	return uintptr(rv), nil
}

// launchGameActivity plays the role of the Java GameActivity after
// initializeNativeCode returns its native handle. The real framework calls the
// registered native methods as the activity moves through its lifecycle; we
// call the same methods in the same order, so the engine starts as far as the
// host platform allows before it reaches an unimplemented import.
//
// This is a launch *attempt*, not a launch: there is no window, EGL context or
// ANativeWindow yet, so the sequence is expected to stop, loudly, at the first
// platform function the surface path needs (ANativeWindow_fromSurface).
func launchGameActivity(handle uintptr) error {
	const cls = "com/google/androidgamesdk/GameActivity"

	env := hostabi.JNIEnvAddr()
	const (
		thiz    = uintptr(0x7b010000) // GameActivity instance
		surface = uintptr(0x7b020000) // android.view.Surface
	)

	call := func(i int, name string, a []uintptr) (uintptr, bool) {
		fn, ok := hostabi.NativeMethod(cls, name)
		if !ok {
			fmt.Printf("launch: step %d %s: NOT REGISTERED\n", i, name)
			return 0, false
		}
		// JNI native ABI: env, thiz, then the Java arguments.
		var args [8]uintptr
		args[0], args[1] = env, thiz
		copy(args[2:], a)
		fmt.Printf("launch: step %d %s(handle=%#x%s) fn=%#x\n", i, name, handle, argNote(a), fn)
		rv := hostabi.CallGuestInt8(fn, args[0], args[1], args[2], args[3],
			args[4], args[5], args[6], args[7])
		fmt.Printf("launch: step %d %s returned %#x\n", i, name, rv)
		return uintptr(rv), true
	}

	fmt.Printf("launch: driving the GameActivity lifecycle around handle %#x\n", handle)

	// onCreate has already happened: initializeNativeCode is its native half.
	// The framework then walks onStart -> onResume -> focus -> insets -> surface.
	steps := []struct {
		name string
		args []uintptr
	}{
		{"onStartNative", []uintptr{handle}},
		{"onResumeNative", []uintptr{handle}},
		{"onWindowFocusChangedNative", []uintptr{handle, 1}}, // hasFocus = true
		{"onWindowInsetsChangedNative", []uintptr{handle}},
		{"onSurfaceCreatedNative", []uintptr{handle, surface}},
		{"onSurfaceChangedNative", []uintptr{handle, surface, windowFormatRGBA8888, 1920, 1080}},
		{"onSurfaceRedrawNeededNative", []uintptr{handle, surface}},
	}

	for i, s := range steps {
		if _, ok := call(i+1, s.name, s.args); !ok {
			return nil
		}
	}
	fmt.Printf("launch: lifecycle drove to onSurfaceRedrawNeeded\n")
	return nil
}

// windowFormatRGBA8888 is ANativeWindow_LegacyFormat::WINDOW_FORMAT_RGBA_8888,
// the format GameActivity reports to onSurfaceChangedNative.
const windowFormatRGBA8888 = 1

// argNote renders the extra Java arguments for the launch trace.
func argNote(a []uintptr) string {
	if len(a) <= 1 {
		return ""
	}
	out := ""
	for _, v := range a[1:] {
		out += fmt.Sprintf(" arg=%#x", v)
	}
	return out
}

// applyTelemetryWorkaround keeps the engine's telemetry natives from recursing
// before their thread context exists. RBXRT_PATCH_REG selects the variant:
//
//	global (default) - force the shared 0x2de3c50 predicate to report a context.
//	family           - only NOP the eight per-wrapper branches; leaves 0x2de3c50
//	                   truthful for the thread-registration helpers.
//	off              - apply nothing; JNI_OnLoad will recurse and crash.
//
// "off" exists so the failure stays reproducible, and every enabled variant
// announces itself: this is a stop-gap, not the engine's natural behaviour.
func applyTelemetryWorkaround(bias uintptr) {
	mode := os.Getenv("RBXRT_PATCH_REG")
	if mode == "" {
		mode = "global"
	}
	switch mode {
	case "off":
		fmt.Fprintf(os.Stderr, "rbxrt: WARNING: telemetry workaround disabled (-off); "+
			"JNI_OnLoad will recurse through the TelemetryBridge wrappers and crash\n")
	case "family":
		if err := patchTelemetryFamily(bias); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: telemetry workaround failed: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "rbxrt: WARNING: telemetry workaround active (family); "+
			"the TelemetryBridge wrappers assume a thread context that does not exist yet\n")
	default:
		if err := patchTelemetryGlobal(bias); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: telemetry workaround failed: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "rbxrt: WARNING: telemetry workaround active (global); "+
			"the engine's thread telemetry registration is bypassed until the task "+
			"scheduler is brought up\n")
	}
}

// telemetrySites lists, in the guest image's own offsets, the conditional
// branches that the TelemetryBridge native family takes when the engine's
// per-thread "context present" check (0x2de3c50) reports false. The family is
// the eight generated Java_..._TelemetryBridge_* wrappers at 0x6d4042c through
// 0x6d40e20: each does
//
//	call 0x2de3c50      ; al = (*(ctx2+0x28) != 0), i.e. "context present?"
//	test %al,%al
//	je   <unregistered> ; builds a closure and calls *envobj->table[i]
//
// With no established context (our bare JNI_OnLoad) al is always 0, so the
// "unregistered" path runs and the wrapper dispatches through the same table
// slot it lives in, recursing into itself until the guest stack is exhausted.
// Each entry below is the address and encoded length of the `je`.
var telemetrySites = []struct {
	off uintptr
	tgt uintptr // the unregistered handler, recorded for diagnostics only
	n   int
}{
	{0x6d40465, 0x6d404ec, 6},
	{0x6d405fb, 0x6d4066a, 2},
	{0x6d40767, 0x6d407e4, 2},
	{0x6d408db, 0x6d4092a, 2},
	{0x6d40a0c, 0x6d40a6a, 2},
	{0x6d40b4f, 0x6d40bd0, 6},
	{0x6d40cdf, 0x6d40d5c, 2},
	{0x6d40e53, 0x6d40ed2, 2},
}

// patchTelemetryFamily replaces each family branch with NOPs so the wrappers
// always take their normal (context present) path. This is deliberately
// narrow: the shared 0x2de3c50 predicate keeps reporting the true state to
// every other caller, in particular the thread-registration helpers around
// 0x2244880 that populate the sampler's per-thread slot table.
func patchTelemetryFamily(bias uintptr) error {
	for _, s := range telemetrySites {
		nops := make([]byte, s.n)
		for i := range nops {
			nops[i] = 0x90
		}
		if err := hostabi.PatchGuest(bias+s.off, nops); err != nil {
			return fmt.Errorf("telemetry site +%#x: %w", s.off, err)
		}
	}
	fmt.Fprintf(os.Stderr, "rbxrt: patched %d telemetry dispatch site(s) to take the context-present path\n", len(telemetrySites))
	return nil
}

// patchTelemetryGlobal is the coarse variant of the workaround: it forces
// 0x2de3c50 itself to report "context present" for every caller. Kept for
// A/B comparison because it also disables the sampler's thread registration.
func patchTelemetryGlobal(bias uintptr) error {
	if err := hostabi.PatchGuest(bias+0x2de3c50, []byte{0xb0, 0x01, 0xc3}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rbxrt: patched 0x2de3c50 -> return true (global)\n")
	return nil
}

// dumpMapsNear prints /proc/self/maps entries for the 512 MiB window above the
// guest image bias, so a guest pc outside the image can be attributed.
func dumpMapsNear(bias uintptr) {
	b, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return
	}
	lo := bias
	hi := bias + 0x20000000
	for _, line := range strings.Split(string(b), "\n") {
		idx := strings.Index(line, "-")
		if idx < 0 {
			continue
		}
		rest := line[idx+1:]
		sp := strings.IndexAny(rest, " ")
		if sp < 0 {
			continue
		}
		var start, end uint64
		fmt.Sscanf(line[:idx], "%x", &start)
		fmt.Sscanf(rest[:sp], "%x", &end)
		if uintptr(end) < lo || uintptr(start) > hi {
			continue
		}
		fmt.Printf("map:  %s\n", line)
	}
}

// debugMem reports whether the verbose memory/stack diagnostics are enabled.
func debugMem() bool { return os.Getenv("RBXRT_DEBUG_MEM") != "" }

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

// gfxEnv reads RBXRT_GFX. "off"/"0"/"none" turns the graphics layer off even
// for --launch; "1"/"on"/"x11"/"wayland" turns it on explicitly; anything else
// (including unset) defers to the command.
func gfxEnv() (on, off bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RBXRT_GFX"))) {
	case "":
		return false, false
	case "0", "off", "none", "no":
		return false, true
	default:
		return true, false
	}
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
