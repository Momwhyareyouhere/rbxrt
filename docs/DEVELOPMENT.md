# rbxrt internals

How rbxrt runs Roblox's Android engine on Linux, and the switches for
debugging it. For using rbxrt, see the [README](../README.md); for the history
of how each piece was reached, see [milestone.md](../milestone.md).

## The idea

Roblox's Android app is a thin Java shell around one native library,
`libroblox.so`. The APK ships an **x86_64** build of it, which a Linux PC can
execute directly. What it cannot do on its own is find what Android provides
around it: Android's C library (bionic), the Java VM it talks to through JNI,
and the platform libraries (EGL/GLES, input, audio, assets).

rbxrt supplies those:

- it loads the library like Android's dynamic linker would,
- it answers the engine's imports with Linux implementations (mostly in Go),
- it plays the part of the Java app: it calls the same native methods, in the
  same order, with the same arguments the APK's Java code uses.

Nothing is emulated and the engine is not modified: rbxrt applies no patches to
the engine image.

## Code layout

| Package | Role |
| --- | --- |
| `internal/apk` | APK reading, binary manifest (AXML), v2/v3 signature verification, Roblox certificate pinning |
| `internal/fetch` | Downloading the APK from the APKPure mirror, verified before use |
| `internal/engine` | ELF analysis of `libroblox.so`: imports, exports, relocations (incl. Android APS2 packed relocations), version |
| `internal/loader` | Maps the ELF, applies relocations and protections, resolves imports, runs constructors, implements `dlopen`/`dlsym` |
| `internal/hostabi` | Everything the engine calls: libc, pthreads/TLS, JNI, NDK APIs, graphics, input, audio |
| `cmd/rbxrt` | The CLI and the "Java app" that drives the engine (boot sequence, input delivery, sessions) |

## Commands

```
rbxrt run <instance> [--update]   fetch if needed, verify, extract and launch
rbxrt fetch <instance>            download the latest APK (verified) into the instance
rbxrt doctor [instance]           check platform, graphics, network and the instance
rbxrt verify <apk>                verify the signature and the Roblox certificate pin
rbxrt manifest <apk>              print the binary manifest
rbxrt inspect <apk>               print signing certificate details
rbxrt engine <lib.so> [import NAME...]   ELF import/export/relocation analysis
rbxrt load <lib.so> [--init] [--jni] [--entry] [--launch] [--gfx] [--trace]
```

`load` runs the stages one at a time, which is useful when something breaks:
`--init` runs the constructors, `--jni` calls `JNI_OnLoad`, `--entry` calls the
app entry point (`GameActivity.initializeNativeCode`), and `--launch` does all of
that and plays the activity lifecycle in a window. `run` is `load --launch` with
the instance's paths filled in.

## How a launch works

1. **Verify and extract.** The APK's whole-file v2/v3 signature is checked and
   its certificate must match Roblox's. `lib/x86_64/libroblox.so` is extracted.
2. **Load.** The loader maps the library, applies its relocations and binds
   every import: Go implementations reached through small trampolines, host
   libraries (Mesa) bound directly, and a trap that names the symbol for
   anything unimplemented. `rbxrt load` prints the unresolved list.
3. **Constructors and `JNI_OnLoad`.** The engine registers its Java natives
   (`RegisterNatives`), which rbxrt records so it can call them later.
4. **App entry.** `GameActivity.initializeNativeCode(...)` with the arguments
   recovered from the APK's dex, then the lifecycle: `onStart`, `onResume`,
   focus, insets, surface created/changed/redraw.
5. **Java boot** (`javaBootKick` in `cmd/rbxrt/load.go`). The calls the Java app
   makes at start: native flags, `nativeGameGlobalInit`, base URL, storage
   directories, device info, client settings (fetched live from Roblox's CDN),
   cookie handler and saved session, `InitWithParams`, `StartAppWithParams`.
6. **Main loop.** Once per frame: deliver input, pump
   `nativeCallMessagesFromMainThread` (the Java main-thread message queue), and
   save the session if cookies changed.

The Java objects the engine reads (device info, platform params, user, locale,
…) are answered from value tables seeded in `cmd/rbxrt/javadata.go`; anything
unanswered is logged once as `has no value` and returns null/0.

Guest code runs on its own mmap'd stack per thread, never on a Go stack, so Go's
stack growth and garbage collector never see a guest frame
(`internal/hostabi/stack.go`). Engine threads are goroutines locked to an OS
thread for their whole life, like pthreads.

## JNI

`internal/hostabi/jni.go` builds the `JNIEnv`/`JavaVM` tables; one dispatcher
serves all 256 entries. Java classes, objects, method and field ids are small
handles. Supported: instance and static calls (direct, `V` and `A` forms),
fields, strings, `byte[]` and `Object[]` arrays, enums (`values()`,
`ordinal()`), boxed primitives, and `String.getBytes` (the engine's
jstring → `std::string` path). Calls nobody answers return null/0 and are
logged once, so a run's log lists exactly what is still missing.

## Graphics

rbxrt does not implement a GPU driver. It loads the host's real libraries with
the real `dlopen` (found in libc through `/proc/self/maps`, since rbxrt does not
use cgo) and binds the engine's `gl*`/`egl*` imports straight to Mesa. No thunk
sits between the engine and the driver.

- **Window.** Native Wayland (xdg-shell, xdg-decoration for the title bar,
  `wl_egl_window`) or X11 (Xlib). `RBXRT_DISPLAY` picks one; the default uses
  Wayland when available and falls back to X11. See `gfx_wayland.go` and
  `gfx_x11.go`.
- **Two translated calls.** `eglGetDisplay` returns the display of the platform
  the window was opened on, and `eglCreateWindowSurface` maps the engine's
  `ANativeWindow` to the `wl_egl_window`/XID Mesa expects (`gfx.go`).
- **Driver.** Mesa's zink (GL on Vulkan) by default; `RBXRT_GFX_BACKEND=mesa`
  uses Mesa's own GL driver, and `MESA_LOADER_DRIVER_OVERRIDE` always wins.

The graphics layer can be tested without the engine:

```
RBXRT_GFX_TEST=1 go test ./internal/hostabi -run TestGraphicsProbe -v
```

## Input

The Android app's own views turn touch and mouse events into calls to the
engine, and rbxrt makes the same calls (`cmd/rbxrt/input.go`):

- mouse: `NativeInputInterface.nativePassMouseMove/Button/Wheel` (positions in
  dp; the runtime reports density 1)
- keys: `NativeGLInterface.nativePassKeyEvent(down, scanCode, keyCode, repeat)`
- text boxes: the engine calls `showKeyboard(box)`/`hideKeyboard()`; while a
  box is focused, edits go to `syncTextboxTextAndCursorPosition2(text, cursor)`
  and Enter to `nativeReturnPressedFromOnScreenKeyboard` + `nativePassText`.

Keyboard layouts come from libxkbcommon (`internal/hostabi/xkb.go`): the
compositor's keymap on Wayland, the X server's on X11, with the locale's compose
table for dead keys. Without it, a US layout is assumed. Wayland key repeat is
done client-side.

## Android platform libraries

`libandroid.so`, `libmediandk.so` and `libaaudio.so` are provided by the runtime
itself: `dlopen` returns a handle, and `dlsym` on it finds a name only if it is
implemented (otherwise NULL, like an older Android).

- **Audio:** AAudio on PulseAudio/PipeWire (`aaudio.go`), in FMOD's callback
  mode, output only. FMOD's Java capability queries are answered so it picks
  AAudio.
- **Media:** a real `AMediaFormat`; `AMediaCodec`/`AImageReader` report no codec,
  like a device without one (`media.go`).
- **Thermal:** `AThermal_*` from the host's thermal sensors (`thermal.go`).
- Also `ALooper`, `AAssetManager` (backed by the APK), `AConfiguration`,
  `ANativeWindow`.

Five `long double` math helpers (`powl`, `roundl`, `llroundl`, `fmal`,
`strtold_l`) are still traps: Android's x86_64 `long double` is 128-bit.

## Sessions

The engine keeps cookies in memory. The Java app registers a cookie handler
(`JNICookieProtocol.updateOnSetCookieHandler`) that the engine calls for every
`Set-Cookie`, and at the next launch restores the cookies with
`nativeSetMultipleCookies(url, cookies)`. rbxrt does the same with
`<instance>/data/session.json` (mode 0600), and before starting signed in asks
`users.roblox.com/v1/users/authenticated` who the login belongs to
(`cmd/rbxrt/session.go`).

Password sign-in stops at Roblox's challenge (`ChallengeHybridWebView`), which
needs an Android web view.

## Security details

The APK is accepted only with a valid v2 or v3 signature over the whole file
(v1/JAR-only signatures are refused), and only if the signing certificate's
SHA-256 is in the built-in list of Roblox's certificates. The v2/v3 structures
were checked against AOSP's `apksig`, and the content digest byte-for-byte
against `ApkSigningBlockUtils.java`. The open-source project Cordial (GPL-3.0)
was consulted for public facts only (certificate digests, the mirror endpoint);
no code was copied.

To audit with other certificates:

```
RBXRT_TRUSTED_CERTIFICATES=/path/to/list.json rbxrt verify roblox.apk
# list.json: ["<sha256-hex>", ...]
```

## Engine builds

The engine embeds its version (e.g. `2.748.0.1527`), which rbxrt prints and
reports to Roblox as the app version. Builds rbxrt has been tested with are
listed in `cmd/rbxrt/profile.go`; others run with a note.

The older build 2.734.0.917 needed a stop-gap: its `TelemetryBridge` natives
recursed until the stack ran out because the engine's task scheduler had not
yet created a per-thread context. Current builds start the scheduler themselves,
so that patch, and every other image patch, has been removed.

## Debugging switches

Everything is quiet by default. All of these are environment variables.

General:

```
RBXRT_DEBUG_JNI=1       JNI call trace (first 300; raise with RBXRT_JNI_TRACE_LIMIT)
RBXRT_DEBUG_GFX=1       window, EGL and per-frame GL call counts
RBXRT_DEBUG_INPUT=1     every mouse/key/text event sent to the engine
RBXRT_DEBUG_AUDIO=1     AAudio streams (open, start, stop, errors)
RBXRT_DEBUG_DL=1        every dlopen/dlsym and its result
RBXRT_DEBUG_SESSION=1   cookie names (never values) and login reports
RBXRT_DEBUG_NET=1       getaddrinfo and socket traffic (=full dumps transfers)
RBXRT_DEBUG_FS=1        file opens and stats by path, with results
RBXRT_DEBUG_MEM=1       guest stacks, guards, code arena, exit-time frames
RBXRT_DEBUG_TLS=1       pthread_getspecific / guest-stack depth trace
RBXRT_DEBUG_LOOPER=1    ALooper trace
RBXRT_DEBUG_ASSET=1     AAssetManager trace
RBXRT_DEBUG_CONFIG=1    AConfiguration trace
RBXRT_WALK=1            dump the guest frame chain at a fatal exit
RBXRT_GOMAXPROCS=<n>    Go scheduler size (default 256)
RBXRT_BREAK=<offsets>   int3 at these image offsets (hex, comma-separated), for gdb
RBXRT_GDB_STOP=<file>   write the image base to <file> and SIGTRAP once
```

`RBXRT_BREAK` needs gdb to start rbxrt itself (Yama `ptrace_scope` 1 forbids
attaching).

Boot:

```
RBXRT_BOOT=1                   play the Java side (set by `run`)
RBXRT_DATA=<dir>               app data directory; the engine's working directory
RBXRT_BOOT_WATCHDOG=<secs>     report stuck threads after this long (30)
RBXRT_LAUNCH_HOLD_SECS=<n>     keep running this long after boot (`run`: forever)
RBXRT_HOLD_DUMP=1              list every guest thread at the end of the hold
RBXRT_OFFLINE=1                use cached client settings instead of fetching them
RBXRT_CLIENT_SETTINGS_FILE=<f> use a saved {"applicationSettings":{...}}
RBXRT_CLIENT_SETTINGS='{...}'  override individual flags
RBXRT_LUA_APP_LOGGED_IN=1      start the Lua app in signed-in mode
RBXRT_START_GAME=1             also drive the game-start path (needs a session)
RBXRT_NO_SESSION=1             don't save or restore the login
```

The engine's own log is `<data>/appData/logs/*_last.log`; its log channels are
fast flags, e.g. `RBXRT_CLIENT_SETTINGS='{"FLogGraphics":"6"}'`.

Graphics and display:

```
RBXRT_DISPLAY=wayland|x11|auto  window system
RBXRT_WINDOW_WIDTH=<n>          window width (1280)
RBXRT_WINDOW_HEIGHT=<n>         window height (720)
RBXRT_GFX=off|1                 disable / force the graphics layer
RBXRT_GFX_BACKEND=mesa          Mesa GL instead of zink
RBXRT_SCREEN_WIDTH_DP=<n>       logical width reported by AConfiguration (960)
RBXRT_SCREEN_HEIGHT_DP=<n>      logical height reported by AConfiguration (540)
```

Language and country come from `LC_ALL`/`LANG` (`en_US.UTF-8` → `en`/`US`).
Shader work: `MESA_SHADER_DUMP_PATH=<dir>` dumps every shader the engine
compiles, and `RBXRT_GFX_TEST=1 RBXRT_GLSL_CORPUS=<dir> go test
./internal/hostabi -run TestGLSLRelaxCorpus` checks them against the driver.

Scripted input, for testing without a person at the window:

```
RBXRT_INPUT_REPLAY="28000:click:640,382;4000:click:640,308;500:key:35"
# steps: <wait ms>:click:x,y | <ms>:drag:x1,y1,x2,y2 | <ms>:key:<evdev code>
```

## Tests

```
go test ./...                       all tests
RBXRT_GFX_TEST=1 go test ./internal/hostabi -run TestGraphicsProbe
RBXRT_NET_TEST=1 go test ./cmd/rbxrt -run TestAuthenticatedUser   (contacts Roblox)
./scripts/fixtures.sh               regenerate APK test fixtures (needs the Android SDK)
```

Design rule: a Java or platform call rbxrt cannot honour fails loudly (logged,
or a trap that names the symbol) rather than faking success.
