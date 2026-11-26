package main

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	selectProfile(b.EngineVersion())

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
			fmt.Printf("graphics: %s window bound to libEGL/libGLESv2 (%dx%d), Mesa will render\n", hostabi.DisplayPlatform(), w, h)
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
	plantBreakpoints(im.Bias)
	gdbStop(im.Bias)

	// The engine keeps its working files (cache/, http/, appData/, exe/)
	// relative to the current directory, as it does on Android where the
	// process runs inside the app's data directory. Move there before any guest
	// code runs, so a run never writes into wherever rbxrt was started from.
	if os.Getenv("RBXRT_BOOT") != "" && (doInit || jni) {
		// Paths given relative to where rbxrt was started must keep meaning
		// the same files after the change of directory.
		for _, k := range []string{"RBXRT_APK", "RBXRT_DATA", "RBXRT_ASSETS"} {
			if v := os.Getenv(k); v != "" && !filepath.IsAbs(v) {
				if abs, err := filepath.Abs(v); err == nil {
					os.Setenv(k, abs)
				}
			}
		}
		if err := ensureDataDir(); err != nil {
			return err
		}
		if err := os.Chdir(rbxrtDataDir()); err != nil {
			return fmt.Errorf("enter data directory: %w", err)
		}
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

	// The Java side the engine expects is answered before any guest code runs:
	// JNI_OnLoad and the constructors already read device parameters, the
	// locale and the clock through JNI (see javadata.go). Gated with the rest
	// of the boot so a run that wants only natural behaviour can turn it off.
	if os.Getenv("RBXRT_BOOT") != "" {
		setJavaStringValues(im)
		seedJavaValues()
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
			return launchGameActivity(im, h)
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

// The fabricated Java references the launch plays. The engine treats them as
// opaque jobjects - it compares them, hands them back to the JNI table, and
// reads the Strings behind them - so each is a distinct non-null sentinel. The
// String sentinels carry real content (see setJavaStringValues): an empty asset
// path would be a wrong answer rather than a missing one.
const (
	javaActivity    = uintptr(0x7b010000) // GameActivity instance
	javaAssetPath   = uintptr(0x7b010001) // String: path to the APK/assets
	javaNativeLib   = uintptr(0x7b010002) // String: native library directory
	javaInternalDir = uintptr(0x7b010003) // String: internal data directory
	javaAssetMgr    = uintptr(0x7b010004) // android.content.res.AssetManager
	javaInsets      = uintptr(0x7b010005) // byte[] (window insets)
	javaConfig      = uintptr(0x7b010006) // android.content.res.Configuration
	javaSurface     = uintptr(0x7b020000) // android.view.Surface
)

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
	fmt.Printf("probing %s (fn=%#x env=%#x)\n", entryPoint, fn, env)
	fmt.Printf("  args: thiz=%#x assetPath=%#x nativeLibDir=%#x internalDir=%#x assets=%#x blob=%#x config=%#x\n",
		javaActivity, javaAssetPath, javaNativeLib, javaInternalDir, javaAssetMgr, javaInsets, javaConfig)
	rv := hostabi.CallGuestInt8(fn, env, javaActivity, javaAssetPath, javaNativeLib, javaInternalDir,
		javaAssetMgr, javaInsets, javaConfig)
	fmt.Printf("initializeNativeCode returned %#x (%d)\n", rv, rv)
	return uintptr(rv), nil
}

// launchGameActivity plays the role of the Java GameActivity after
// initializeNativeCode returns its native handle. The real framework calls the
// registered native methods as the activity moves through its lifecycle; we
// call the same methods in the same order, so the engine starts as far as the
// host platform allows before it reaches an unimplemented import.
//
// The window already exists by this point (InitGraphics created it), so the
// sequence's job is to tell the engine about that window. Afterwards we hold
// the process open: the engine starts acting on the surface asynchronously, and
// exiting the instant step 7 returns would end it before it ever reached EGL.
func launchGameActivity(im *loader.Image, handle uintptr) error {
	const cls = "com/google/androidgamesdk/GameActivity"

	env := hostabi.JNIEnvAddr()
	// The synthetic Surface has to be reachable as a window: the engine may
	// hand it to eglCreateWindowSurface without ANativeWindow_fromSurface.
	hostabi.BindGuestSurface(javaSurface)

	// The dimensions the framework would report are the real window's, so the
	// engine's render target matches what eglSwapBuffers ends up in.
	w, h := hostabi.WindowSize()
	if w == 0 || h == 0 {
		w, h = 1920, 1080
	}

	call := func(i int, name string, a []uintptr) (uintptr, bool) {
		fn, ok := hostabi.NativeMethod(cls, name)
		if !ok {
			fmt.Printf("launch: step %d %s: NOT REGISTERED\n", i, name)
			return 0, false
		}
		// JNI native ABI: env, thiz, then the Java arguments.
		var args [8]uintptr
		args[0], args[1] = env, javaActivity
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
		{"onSurfaceCreatedNative", []uintptr{handle, javaSurface}},
		{"onSurfaceChangedNative", []uintptr{handle, javaSurface, windowFormatRGBA8888,
			uintptr(w), uintptr(h)}},
		{"onSurfaceRedrawNeededNative", []uintptr{handle, javaSurface}},
	}

	for i, s := range steps {
		if _, ok := call(i+1, s.name, s.args); !ok {
			return nil
		}
	}
	fmt.Printf("launch: lifecycle drove to onSurfaceRedrawNeeded\n")
	pump := javaBootKick(im)
	// Mouse and keyboard are delivered on this (the "Java main") thread
	// between message pumps, where Roblox's Java input layer delivers them.
	if step := inputDriver(im); step != nil && hostabi.GraphicsEnabled() {
		inner := pump
		pump = func() int {
			step()
			if inner != nil {
				return inner()
			}
			return 0
		}
	}
	holdForRendering(im, pump)
	return nil
}

// bootWatchdog reports where the Java handshake is stuck. It returns a stop
// function; the caller defers it so a normal return disarms the check. On
// expiry it dumps every guest thread's last host call (the exact instruction
// the thread is in, or called from) and exits, because a startup call that
// never returns cannot make progress by waiting longer.
func bootWatchdog(im *loader.Image) func() {
	secs := 30
	if s := os.Getenv("RBXRT_BOOT_WATCHDOG"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			secs = n
		} else {
			fmt.Printf("boot: bad RBXRT_BOOT_WATCHDOG %q, using 30\n", s)
		}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-time.After(time.Duration(secs) * time.Second):
		}
		fmt.Fprintf(os.Stderr, "boot: handshake still running after %ds; guest threads:\n", secs)
		dumpGuestThreads(im, "boot")
		os.Exit(3)
	}()
	return func() { close(done) }
}

// javaBootKick calls into the Roblox-specific JNI entry points the app's Java
// side is responsible for, which nothing in this runtime has ever done.
//
// The picture from the traces: JNI_OnLoad only initializes the library, and the
// Java app afterwards drives the engine through NativeGLInterface - the exported
// symbols the APK's classes call are right there in the image. With none of
// them called, the engine consumes the GameActivity lifecycle commands and then
// idles in its looper, never reaching EGL - exactly what we measure.
//
// The argument lists below are recovered from the APK's dex, not guessed: the
// startup sequence the Java side drives is nativeGameGlobalInit() (no
// arguments - the context hypothesis was wrong), then
// nativeAppBridgeV2InitWithParams(InitParams) and
// nativeAppBridgeV2StartAppWithParams(StartAppParams) with AutoValue objects
// built by the app. The two param objects are fakes: the engine reads their
// fields through JNI, which answers from the value tables in jobject.go and
// reports every request it has no answer for. It is gated by RBXRT_BOOT so a
// run that wants only the natural behaviour can turn it off.
//
// The returned function is the frame pump, nil when it is unavailable.
func javaBootKick(im *loader.Image) (pump func() int) {
	if os.Getenv("RBXRT_BOOT") == "" {
		return nil
	}
	env := hostabi.JNIEnvAddr()
	// The natives are static methods of the Java interface the APK declares; JNI
	// passes their class as the second argument.
	cls := hostabi.FindClassHandle("com/roblox/engine/jni/NativeGLInterface")
	const prefix = "Java_com_roblox_engine_jni_NativeGLInterface_"

	// Everything the engine reads back out of Java during this startup was
	// answered before JNI_OnLoad (see the seeding in cmdLoad and javadata.go).

	call := func(name string, arg uintptr) {
		fn, ok := im.Exports[prefix+name]
		if !ok {
			fmt.Printf("boot: %s is not exported\n", name)
			return
		}
		fmt.Printf("boot: calling %s(arg=%#x)\n", name, arg)
		if os.Getenv("RBXRT_DEBUG_TLS") != "" {
			hostabi.TraceTLSCalls(500)
		}
		rv := hostabi.CallGuestInt3(fn, env, cls, arg)
		fmt.Printf("boot: %s returned %#x\n", name, rv)
	}
	callOther := func(class, name string, arg uintptr) {
		symbol := "Java_" + strings.ReplaceAll(class, "/", "_") + "_" + name
		fn, ok := im.Exports[symbol]
		if !ok {
			fmt.Printf("boot: %s is not exported\n", name)
			return
		}
		classHandle := hostabi.FindClassHandle(class)
		fmt.Printf("boot: calling %s(arg=%#x)\n", name, arg)
		rv := hostabi.CallGuestInt3(fn, env, classHandle, arg)
		fmt.Printf("boot: %s returned %#x\n", name, uintptr(rv))
	}
	callArgs := func(class, name string, args ...uintptr) uintptr {
		symbol := "Java_" + strings.ReplaceAll(class, "/", "_") + "_" + name
		fn, ok := im.Exports[symbol]
		if !ok {
			fmt.Printf("boot: %s is not exported\n", name)
			return 0
		}
		var a [8]uintptr
		copy(a[:], args)
		fmt.Printf("boot: calling %s\n", name)
		rv := hostabi.CallGuestInt8(fn, a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7])
		fmt.Printf("boot: %s returned %#x\n", name, uintptr(rv))
		return uintptr(rv)
	}

	// A startup native that never returns looks exactly like a slow one unless
	// something says where it is. The hold's watchdog only starts after this
	// handshake, so give the handshake its own: report every guest thread at its
	// last host call and stop, instead of hanging until the outer timeout.
	stopWatchdog := bootWatchdog(im)
	defer stopWatchdog()

	// Android initializes the native flag provider from its preferences in
	// Application.onCreate, before any activity native runs. Since 2.748,
	// nativeGameGlobalInit hands the app-bridge global init to a worker that
	// blocks in RbxStorage::getStorageInterface until fast flags are loaded,
	// so this must come first. This clean launch has no persisted overrides,
	// so pass an empty fake String array.
	callOther("com/roblox/client/flags/FlagJniInterface", "nativeInitializeNativeFlags",
		hostabi.NewFakeObject("[Ljava/lang/String;"))
	call("nativeGameGlobalInit", 0)
	call("nativeUpdateAdapterInit", 0)
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetBaseUrl",
		env, hostabi.FindClassHandle("com/roblox/engine/jni/NativeSettingsInterface"),
		hostabi.NewJavaString("https://www.roblox.com"), hostabi.NewJavaString("https://api.roblox.com"))
	callOther("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetRobloxChannel",
		hostabi.NewJavaString("production"))
	// The app's storage locations, as Android's Context reports them
	// (getFilesDir, getCacheDir, getExternalFilesDir). Under the live flags
	// RbxStorage needs a cache directory and throws "Path does not exist"
	// (an RBXCRASH) if it was never set.
	settingsCls := hostabi.FindClassHandle("com/roblox/engine/jni/NativeSettingsInterface")
	dirs := map[string]string{}
	for _, d := range []string{"files", "cache", "external"} {
		dirs[d] = filepath.Join(rbxrtDataDir(), d)
		if err := os.MkdirAll(dirs[d], 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: create %s directory: %v\n", d, err)
		}
	}
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetFilesDirectory",
		env, settingsCls, hostabi.NewJavaString(dirs["files"]))
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetCacheDirectory",
		env, settingsCls, hostabi.NewJavaString(dirs["cache"]))
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetExternalDirectory",
		env, settingsCls, hostabi.NewJavaString(dirs["external"]))
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetBaseDataDirectories",
		env, settingsCls, hostabi.NewJavaString(dirs["files"]), hostabi.NewJavaString(dirs["cache"]))
	// The device description Java builds from android.os.Build. The client
	// parses osVersion as the Android API level ("Android API %d"); unset, it
	// reads 0 and the app shows "Roblox no longer supports Android 6 and 7".
	// The values describe this runtime, not any real phone model.
	const classDeviceInfo = "com/roblox/engine/jni/model/DeviceInfo"
	w, h := hostabi.WindowSize()
	for field, value := range map[string]string{
		"osVersion":         strconv.Itoa(androidSDKInt),
		"deviceName":        "rbxrt",
		"manufacturer":      "rbxrt",
		"appVersion":        activeProfile.version,
		"country":           deviceCountry(),
		"displayResolution": fmt.Sprintf("%dx%d", w, h),
		"networkType":       "wifi",
		"appBuildVariant":   "Production",
		"deviceSku":         "",
		"socModel":          "x86_64",
		"testDeviceName":    "",
	} {
		hostabi.SetFakeField(classDeviceInfo, field, hostabi.NewJavaString(value))
	}
	callArgs("com/roblox/engine/jni/NativeSettingsInterface", "nativeSetDeviceInfo",
		env, settingsCls, hostabi.NewFakeObject(classDeviceInfo))
	callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeInitClientSettings",
		env, cls, hostabi.NewJavaString(clientSettingsJSON()),
		hostabi.NewJavaString(""), hostabi.NewJavaString("production"))
	// Java follows the settings with the post-settings initialization: it
	// passes the app's recent ApplicationExitInfo records (none on a fresh
	// install: an empty java.util.List, see javadata.go), and the engine then
	// applies the loaded settings to the app-bridge components
	// (appbridge_adjust_settings). Without it the flags never take effect.
	callArgs("com/roblox/engine/jni/NativeGLInterface", "nativePostClientSettingsLoadedInitialization3",
		env, cls, hostabi.NewFakeObject("java/util/List"))
	callOther("com/roblox/client/startup/MainGameActivity", "nativeSetAssetPath",
		hostabi.NewJavaString(filepath.Join(rbxrtDataDir(), "assets", "content")))
	// Restore the saved login (session.go) before the app bridge starts, as
	// the Java app does from its CookieManager.
	var session *sessionKeeper
	signedIn := false
	if sessionEnabled() {
		session = newSessionKeeper(func(name string, args ...uintptr) uintptr {
			fn, ok := im.Exports["Java_com_roblox_engine_jni_NativeSettingsInterface_"+name]
			if !ok {
				return 0
			}
			var a [8]uintptr
			a[0], a[1] = env, settingsCls
			copy(a[2:], args)
			return uintptr(hostabi.CallGuestInt8(fn, a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7]))
		})
		session.watchLogin()
		session.register(im.Exports, env)
		var user sessionUser
		if user, signedIn = session.restore(); signedIn {
			seedSessionUser(user)
		}
	}
	initParams := hostabi.NewFakeObject(classInitArgs)
	call("nativeAppBridgeV2InitWithParams", initParams)
	// Disable the generated telemetry sink itself during the pre-scheduler
	// phase. Entering its exception machinery is what produces the int3 at
	// 0x6d43424; telemetry is not required to create the graphics context.
	stubReturn0(im.Bias, activeProfile.telemetrySink, "telemetry sink")
	nopCall(im.Bias, activeProfile.keyEventProbe, "key-event probe")
	callArgs("com/roblox/engine/jni/NativeGLInterface", "setTaskSchedulerBackgroundMode",
		env, cls, 0, hostabi.NewJavaString("ASMA.start"))
	call("nativeAppBridgeV2StartAppWithParams", hostabi.NewFakeObject(classStartApp))
	if fn, ok := im.Exports[prefix+"nativeCallMessagesFromMainThread"]; ok {
		hostabi.CallGuestInt3(fn, env, cls, 0)
	}
	// MainGameActivity dispatches this through its executor immediately after
	// StartApp. It installs the Maquettes/render integration used by the native
	// app bridge; without the Java executor, the native bridge remains in its
	// pre-render state.
	callArgs("com/roblox/engine/jni/NativeGLInterface", "initMaquettesSDK",
		env, cls, javaActivity)
	startGame := os.Getenv("RBXRT_START_GAME") != ""
	if !startGame {
		// Java sends a second surface/platform update after the app bridge starts;
		// this is the notification that normally drives the app render view.
		nopCall(im.Bias, activeProfile.luaStartKeyProbe, "Lua-start key probe")
		// Java calls this once a user has logged in ("initializeLuaAppWith
		// LoggedInUser"). The runtime has no Roblox session, so by default it
		// launches logged out, as a fresh install does: the Lua app then shows
		// its own login screen (FIntLuaAppLoginMethod=1 in the live settings)
		// instead of a logged-in home whose every request fails with 401.
		// RBXRT_LUA_APP_LOGGED_IN=1 restores the call.
		// A restored login (session.go) starts the app signed in too.
		if os.Getenv("RBXRT_LUA_APP_LOGGED_IN") != "" || signedIn {
			call("nativeAppBridgeStartLuaAppDM", 0)
		}
	}
	callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeAppBridgeV2SendAppEventOnAppReady",
		env, cls, hostabi.NewJavaString(""), hostabi.NewJavaString(""),
		hostabi.NewJavaString(""), hostabi.NewJavaString(""))
	callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeScheduleOnFirstFrame",
		env, cls, hostabi.NewFakeObject("java/lang/Runnable"))
	if startGame {
		// StartGame has the same synthetic key-event probe as StartApp, but at
		// its own call site. There is no Android input queue in the host.
		nopCall(im.Bias, activeProfile.startGameKeyProbe, "StartGame key-event probe")
		call("nativeAppBridgeV2StartGameWithParam",
			hostabi.NewFakeObject("com/roblox/engine/jni/autovalue/StartGameParams"))
		callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeAppBridgeV2UpdateSurfaceGameWithPlatformParams",
			env, cls, javaSurface, hostabi.NewFakeObject(classPlatform))
		call("nativeAppBridgeV2SendAppEventOnGameLoaded", 0)
		callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeAppBridgeV2ResumeGameWithPlatformParams",
			env, cls, hostabi.NewFakeObject(classPlatform))
		call("nativeOnExperienceSessionResume", 0)
	}

	pumpName := prefix + "nativeCallMessagesFromMainThread"
	fn, ok := im.Exports[pumpName]
	if !ok {
		fmt.Printf("boot: %s is not exported\n", pumpName)
		return nil
	}
	fmt.Printf("boot: will pump %s once per frame\n", pumpName)
	updates := 0
	return func() int {
		if session != nil {
			session.save()
		}
		if updates < 1 {
			updates++
			callArgs("com/roblox/engine/jni/NativeGLInterface", "nativeAppBridgeV2UpdateSurfaceAppWithPlatformParams",
				env, cls, javaSurface, hostabi.NewFakeObject(classPlatform))
		}
		return hostabi.CallGuestInt3(fn, env, cls, 0)
	}
}

// holdForRendering keeps the process open after the lifecycle: the engine
// reacts to the surface asynchronously, so exiting the instant step 7 returns
// would end it before it ever reached EGL. RBXRT_LAUNCH_HOLD_SECS bounds the
// hold in seconds (default 15, 0 restores the old exit-immediately behaviour),
// the frame pump (when the Java handshake is enabled) is called every ~16ms,
// and the EGL counters report what, if anything, rendering got to.
// holdForever makes holdForRendering run until the process is stopped
// (rbxrt run), instead of for RBXRT_LAUNCH_HOLD_SECS.
var holdForever bool

func holdForRendering(im *loader.Image, pump func() int) {
	secs := 15
	if s := os.Getenv("RBXRT_LAUNCH_HOLD_SECS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			secs = n
		} else {
			fmt.Printf("launch: bad RBXRT_LAUNCH_HOLD_SECS %q, using 15\n", s)
		}
	}
	if secs == 0 && !holdForever {
		return
	}
	if holdForever {
		fmt.Printf("launch: running; press Ctrl-C to quit\n")
	} else {
		fmt.Printf("launch: holding %ds for the engine to act on the surface\n", secs)
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)

	// The sampler runs on a goroutine of its own: a pump that blocks inside
	// the engine must not also blind the diagnostics, which exist precisely
	// to say what a blocked thread is waiting for.
	done := make(chan struct{})
	go func() {
		last := make([]int64, 4)
		seen := map[uintptr]uintptr{}
		tick := time.NewTicker(512 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			inits, ctxs, cur, swaps := hostabi.EGLCounts()
			// Normal play (rbxrt run) prints no per-frame progress lines.
			quiet := holdForever && os.Getenv("RBXRT_DEBUG_GFX") == ""
			if !quiet && (inits != last[0] || ctxs != last[1] || cur != last[2] || swaps != last[3]) {
				fmt.Printf("launch: EGL counts: initialize=%d context=%d makeCurrent=%d swap=%d\n",
					inits, ctxs, cur, swaps)
				last[0], last[1], last[2], last[3] = inits, ctxs, cur, swaps
			}
			for _, ts := range hostabi.GuestThreadsSnapshot() {
				if os.Getenv("RBXRT_DEBUG_THREADS") == "" || seen[ts.Base] == ts.RetPC {
					continue
				}
				seen[ts.Base] = ts.RetPC
				// depth 0 means the thread is running engine code right now and
				// the name is where it last called into the host.
				what := im.CallSiteName(ts.RetPC)
				if what == "" {
					what = fmt.Sprintf("image+%#x", hostabi.GuestImageOffset(ts.RetPC))
				}
				fmt.Printf("launch: thread %#x last-host=%s depth=%d\n", ts.Base, what, ts.Depth)
			}
		}
	}()

	pumps, blocked := 0, 0
	overdue := make(chan struct{})
	go func() {
		if holdForever {
			return // no deadline to overrun
		}
		time.Sleep(time.Duration(secs+3) * time.Second)
		select {
		case <-overdue:
			return
		default:
		}
		// A pump call that never returns would look like a silent hang, with
		// the sampler still ticking: say where every goroutine and guest
		// thread is, then stop rather than burn the clock.
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "launch: hold deadline passed with the loop still running; goroutines:\n%s", buf[:n])
		for _, ts := range hostabi.GuestThreadsSnapshot() {
			what := im.CallSiteName(ts.RetPC)
			if what == "" {
				what = fmt.Sprintf("image+%#x", hostabi.GuestImageOffset(ts.RetPC))
			}
			fmt.Fprintf(os.Stderr, "launch: guest thread %#x last-host=%s depth=%d\n",
				ts.Base, what, ts.Depth)
		}
		os.Exit(3)
	}()
	for holdForever || time.Now().Before(deadline) {
		time.Sleep(16 * time.Millisecond) // ~60 Hz, a frame's worth of pumping
		if pump != nil {
			start := time.Now()
			if pump() != 0 {
				pumps++
			}
			if d := time.Since(start); d > 500*time.Millisecond {
				blocked++
				if !holdForever || os.Getenv("RBXRT_DEBUG_GFX") != "" {
					fmt.Printf("launch: pump call did not return for %v (engine blocked?)\n", d.Round(100*time.Millisecond))
				}
			}
		}
	}
	close(done)
	close(overdue)
	if pump != nil {
		fmt.Printf("launch: pumped nativeCallMessagesFromMainThread %d times (%d blocked calls)\n",
			pumps, blocked)
	}
	inits, ctxs, cur, swaps := hostabi.EGLCounts()
	fmt.Printf("launch: hold over; EGL counts: initialize=%d context=%d makeCurrent=%d swap=%d\n",
		inits, ctxs, cur, swaps)
	if os.Getenv("RBXRT_HOLD_DUMP") != "" {
		dumpGuestThreads(im, "hold")
	}
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
// The per-build sites live in buildProfile.telemetrySites (profile.go).

// patchTelemetryFamily replaces each family branch with NOPs so the wrappers
// always take their normal (context present) path. This is deliberately
// narrow: the shared 0x2de3c50 predicate keeps reporting the true state to
// every other caller, in particular the thread-registration helpers around
// 0x2244880 that populate the sampler's per-thread slot table.
func patchTelemetryFamily(bias uintptr) error {
	sites := activeProfile.telemetrySites
	if len(sites) == 0 {
		return fmt.Errorf("no telemetry sites located in build %s", activeProfile.version)
	}
	for _, s := range sites {
		nops := make([]byte, s.n)
		for i := range nops {
			nops[i] = 0x90
		}
		if err := hostabi.PatchGuestChecked(bias+s.off, nops, isJe); err != nil {
			return fmt.Errorf("telemetry site +%#x: %w", s.off, err)
		}
	}
	fmt.Fprintf(os.Stderr, "rbxrt: patched %d telemetry dispatch site(s) to take the context-present path\n", len(sites))
	return nil
}

// patchTelemetryGlobal is the coarse variant of the workaround: it forces
// 0x2de3c50 itself to report "context present" for every caller. Kept for
// A/B comparison because it also disables the sampler's thread registration.
func patchTelemetryGlobal(bias uintptr) error {
	off := activeProfile.telemetryPredicate
	if off == 0 {
		return fmt.Errorf("telemetry predicate not located in build %s", activeProfile.version)
	}
	if err := hostabi.PatchGuest(bias+off, []byte{0xb0, 0x01, 0xc3}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rbxrt: patched %#x -> return true (global)\n", off)
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

// plantBreakpoints writes an int3 at each guest-image offset listed in
// RBXRT_BREAK (comma-separated hex, e.g. 0x23f07a3), for running under gdb:
// the process stops there with SIGTRAP and the guest's registers intact. The
// instruction at the site is destroyed, so this is for inspection only.
func plantBreakpoints(bias uintptr) {
	spec := os.Getenv("RBXRT_BREAK")
	if spec == "" {
		return
	}
	for _, f := range strings.Split(spec, ",") {
		off, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(f), "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: RBXRT_BREAK: bad offset %q\n", f)
			continue
		}
		if err := hostabi.PatchGuest(bias+uintptr(off), []byte{0xcc}); err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: RBXRT_BREAK %#x: %v\n", off, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "rbxrt: breakpoint at image+%#x (%#x)\n", off, bias+uintptr(off))
	}
}

// gdbStop supports breakpoints from gdb on the guest image, whose load address
// is only known once it is mapped. With RBXRT_GDB_STOP=<file> the bias is
// written to <file> as hex, then the process raises SIGTRAP once, before any
// guest code runs: a gdb script reads the file, sets breakpoints at
// bias+offset, and continues with the signal suppressed. Without a debugger
// attached the SIGTRAP terminates the process, so use it only under gdb.
func gdbStop(bias uintptr) {
	path := os.Getenv("RBXRT_GDB_STOP")
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%#x\n", bias)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: RBXRT_GDB_STOP: %v\n", err)
		return
	}
	syscall.Tgkill(os.Getpid(), syscall.Gettid(), syscall.SIGTRAP)
}

// dumpGuestThreads prints every guest thread's most recent host call and its
// guest frame chain (image offsets), for finding what a stuck engine waits on.
func dumpGuestThreads(im *loader.Image, tag string) {
	for _, ts := range hostabi.GuestThreadsSnapshot() {
		what := im.CallSiteName(ts.RetPC)
		if what == "" {
			what = fmt.Sprintf("image+%#x", hostabi.GuestImageOffset(ts.RetPC))
		}
		fmt.Fprintf(os.Stderr, "%s: guest thread %#x (pthread %#x) last-host=%s depth=%d rbp=%#x\n",
			tag, ts.Base, ts.Thread, what, ts.Depth, ts.RBP)
		for i, ret := range hostabi.GuestFrameChain(ts.RBP, ts.StackTop, 24) {
			fmt.Fprintf(os.Stderr, "  guestframe[%d] pc=%#x image+%#x\n",
				i, ret, hostabi.GuestImageOffset(ret))
		}
	}
}

// deviceCountry is the two-letter country from LC_ALL/LANG (e.g. en_US.UTF-8
// -> US), as the AConfiguration layer derives it, or "US".
func deviceCountry() string {
	for _, k := range []string{"LC_ALL", "LANG"} {
		v := os.Getenv(k)
		if i := strings.IndexByte(v, '_'); i >= 0 && len(v) >= i+3 {
			return strings.ToUpper(v[i+1 : i+3])
		}
	}
	return "US"
}
