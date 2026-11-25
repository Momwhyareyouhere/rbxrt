# rbxrt — from-scratch Android-Roblox runtime for Linux

A native Android-Roblox client runtime for Linux. No Wine, no Android
emulator, no VM. Open-source and auditable, written in Go.

Goals:

1. **Engine runtime** — load and execute Roblox's native Android binary
   (`libroblox.so`) on Linux (this milestone, in progress).
2. **Launcher & settings** — multi-instance, first-run APK download or
   user-import, per-instance settings.
3. **Plugins, Flatpak, AppImage** — packaging and extension points.

## Security model

Before anything from an APK is executed it must be **cryptographically proven
to be signed by Roblox**. The engine refuses v1 (JAR) scheme-only signatures and
verifies whole-file integrity with the v2/v3 APK Signature Scheme, then checks
the signing certificate's SHA-256 against a built-in list of Roblox's signing
certificates.

Everything here is based on public formats (APK signing block, AXML, ELF) and
public facts. The byte layout of the v2/v3 signer structures was verified
against AOSP's `apksig` sources, and the content-digest algorithm was
verified byte-for-byte against `ApkSigningBlockUtils.java`. The reference
open-source project Cordial (GPL-3.0) was consulted only for public facts
(certificate digests, APKPure mirror endpoint); no code was copied.

### Trusted certificates

Built-in Roblox signing certificate SHA-256:

```
44932ea35a17a267372d71b54d1a0cb3da0dca5113e94406ae2fe18090ba1477
```

Cross-checked against genuine `base.apk` (Roblox 2.734.0.917, x86_64) via the
Cordial trust file. Override for testing/auditing:

```
RBXRT_TRUSTED_CERTIFICATES=/path/to/list.json rbxrt verify roblox.apk
# list.json: ["<sha256-hex>", ...]
```

## Milestone status

- [x] `internal/apk` — APK set reading (base+split), binary-manifest (AXML)
      parser, v2/v3 signature verification, content-digest integrity, Roblox
      certificate pinning. Fixtures generated with the real Android SDK tools
      (`aapt2`, `zipalign`, `apksigner`) and verifiable with `apksigner`.
- [x] `internal/engine` — ELF import/export/relocation analysis of
      `libroblox.so` (SysV + Android APS2 packed relocs), dynamic init/fini.
- [x] `internal/hostabi` — host symbol provider: executable ABI trampolines to
      native Linux libc/math/syscalls, data objects, dl hooks, a JNI environment,
      and pthread/TLS. Unknown strong functions bind to a trap that aborts
      naming the symbol.
- [x] `internal/loader` — map PT_LOAD, apply relocations, per-page protections,
      RELRO, resolve imports against the image + host, run constructors
      (`rbxrt load <lib.so> [--init]`).
- [x] constructors of the genuine engine library complete (3678 init functions).
- [x] `JNI_OnLoad` runs to completion and returns `JNI_VERSION_1_6`
      (`rbxrt load <lib.so> --init --jni`).
- [x] app-entry probe: `--entry` drives the real entry point, the GameActivity
      native `Java_com_google_androidgamesdk_GameActivity_initializeNativeCode`
      (see below). It registers GameActivity's 24 natives, prepares the main
      looper, reads the asset manager and device configuration, and returns a
      non-null JNI native handle (`initializeNativeCode returned 0x...`).
- [x] graphics platform — `ANativeWindow` plus EGL/GLES. The runtime opens a
      real X11 window and drives the host's Mesa driver (`libEGL`/`libGLESv2`)
      directly; see *Graphics platform* below.
- [ ] Android NDK platform layer — `ALooper`, `AAssetManager`,
      `AConfiguration` and graphics are implemented; audio, media and networking
      remain. These are what the next entry point (the engine's own native
      threads and renderer) needs first.
- [ ] engine task scheduler and JNI method table — the next entry point, and the
      prerequisite for removing the telemetry stop-gap below.

Guest code runs on a dedicated mmap'd stack per thread rather than the Go stack,
so the Go runtime's stack growth and GC never walk a guest frame; host functions
the guest calls execute on that same stack. See `internal/hostabi/stack.go`.

### App-entry probe

`JNI_OnLoad` only initializes the library. The application starts when the Java
`GameActivity` calls its native method:

```
rbxrt load <lib.so> --entry        # implies --init and --jni
```

`--entry` fabricates a JNI environment, then invokes
`Java_com_google_androidgamesdk_GameActivity_initializeNativeCode`. Its Java
prototype was recovered from the APK's `classes2.dex`:

```java
long initializeNativeCode(String assetPath, String nativeLibDir,
                          String internalDataDir, AssetManager assets,
                          byte[] insets, Configuration config);
```

With `env` and `thiz` prepended that is eight arguments — six in registers, two
on the stack (`CallGuestInt8` / `callGuestOnGuestStack8`). Every argument is a
distinct non-null sentinel. The probe prints the returned native handle:

```
initializeNativeCode returned 0x7fd700125300 (140561395897088)
```

The engine reaches this by way of the platform layer: `ALooper_forThread`
(prepared looper), `ALooper_addFd` (the engine's own wake pipe), the NDK asset
manager (`AAssetManager_fromJava`, sourced from `RBXRT_APK`), and the device
configuration (`AConfiguration_new` / `AConfiguration_fromAssetManager`, derived
from `LANG` and the `RBXRT_SCREEN_*_DP` overrides). It then prepares a second
looper for its own thread and registers a second fd before returning.

### Launch attempt

```
rbxrt load <lib.so> --launch     # implies --init --jni --entry
```

`--launch` goes one step further than the probe: after `initializeNativeCode`
returns its native handle, the runtime plays the role of the Java `GameActivity`
and calls the registered lifecycle natives in the order the framework would —
`onStartNative`, `onResumeNative`, `onWindowFocusChangedNative`,
`onWindowInsetsChangedNative`, `onSurfaceCreatedNative`,
`onSurfaceChangedNative`, `onSurfaceRedrawNeededNative`. The native methods and
their signatures are taken from the engine's own `RegisterNatives` call, so the
sequence cannot drift from the library.

Today this is a launch *attempt*, not a launch. The engine accepts
`onStartNative` and then terminates itself with exit code 1000. That happens
*before* it makes any EGL or GL call: `--launch` now brings up the full graphics
platform first (see below), and the engine still never reaches `eglGetDisplay`.

The exit site is now known exactly. Each host thunk records the guest return
address *and frame pointer* of its call site in the current goroutine's guest
stack, just below its low bound (`invokeGuest` keeps `g.stack.lo` pointed there
while guest code runs, so the thunk derives the slot from `g` alone). Because the
slot is per-goroutine, a concurrent thread can no longer clobber it, and the
report is stable across runs instead of naming a random `pthread_*` call. The
slot is only inside mapped memory while `g.stack.lo` really is a guest stack's
base, so the few host-to-thunk paths that run outside guest code (`CallHostRaw`
and the thunk probes) install that context first with `withGuestStackContext`.
`exit(1000)` comes from `image+0x1e95d5b`: the engine's
`RBX_on* callback re-entrancy violation` guard, entered with callback class 1 —
the error/logging entry point at `image+0x6ce8266`, which formats a message and
calls the engine's logger. The guard makes re-entrant class-1 entry fatal
(`_Exit(1000)`) rather than benignly skipped. `RBXRT_WALK=1` dumps the guest RBP
chain at the fatal call and shows why: the stack is in **unbounded recursion**
through the `TelemetryBridge` wrapper family (`+0x6d40a4a`, `+0x6d40b06`) and the
`std::function`-style invoker at `+0x2de36f0` (`+0x2de37a6`). Those wrappers are
the ones the `RBXRT_PATCH_REG` workaround forces down their "context present"
path, so the engine believes it has thread telemetry state it does not have.

The remaining gap is therefore the Java side and the engine's own task
scheduler/thread telemetry context — the engine expects state a real
`GameActivity`/Roblox client would have supplied (it already reports
`[JNIMain] DeviceStaticParams is null.` during `JNI_OnLoad`).

### Graphics platform

The runtime does not reimplement a GPU driver and does not use Wine or a VM. It
loads the *host's* real window system and GPU stack and binds the engine's
imports to it:

- **Host dynamic linking.** `rbxrt` is dynamically linked (the C library is
  mapped because the Go runtime uses it for DNS/user lookups), so the real
  `dlopen`/`dlsym` are present in the process. `internal/hostabi/hostdl.go`
  locates libc in `/proc/self/maps`, reads its own dynamic symbol table to find
  those entry points, and calls them through the same guest-call trampoline used
  everywhere else. `libX11.so.6`, `libEGL.so.1` and `libGLESv2.so.2` are opened
  with `RTLD_NOW|RTLD_GLOBAL`.
- **Real window.** `InitGraphics` calls `XOpenDisplay`, `XCreateSimpleWindow`,
  `XMapWindow` and friends, yielding a genuine X11 window (an XID) on the host
  display.
- **Direct binding.** Every `gl*`/`egl*` import the engine has is bound straight
  into its GOT to the Mesa implementation (see `RegisterRawSymbol` and the
  resolver hook in `internal/hostabi/registry.go`). No thunk sits between the
  guest and the driver, which is what makes floating-point parameters and the
  wider argument lists (`glTexImage2D` takes nine) work at all.
- **The two that need translating.** `eglGetDisplay` returns the X11 platform
  display (via `eglGetPlatformDisplayEXT`) so the platform is deterministic even
  on a Wayland session; `eglCreateWindowSurface` maps the engine's
  `ANativeWindow` to the X11 window Mesa expects. `ANativeWindow_fromSurface`
  and the `getWidth`/`getHeight`/`acquire`/`release` accessors are implemented in
  Go (`internal/hostabi/gfx.go`).

`--launch` enables graphics automatically; `RBXRT_GFX=1` forces it for
`--entry`, and `RBXRT_GFX=off` disables it. If it cannot come up, the engine's
graphics calls still abort by name — the runtime never pretends to render.

The layer is verified without the engine by a probe that opens the window,
creates a GLES 3.2 context and swaps a frame:

```
RBXRT_GFX_TEST=1 go test ./internal/hostabi -run TestGraphicsProbe -v
```

On this host it reports the real driver (e.g. `Mesa Intel(R) UHD Graphics`),
`OpenGL ES 3.2 Mesa`, `EGL 1.5`, and renders one frame into the X11 window.

### Engine builds

Every guest-image offset the runtime patches belongs to one engine build, so
offsets live in per-build profiles (`cmd/rbxrt/profile.go`) keyed by the client
version string the engine embeds (e.g. `2.748.0.1527`); `rbxrt load` prints the
detected build. Each patch also checks the instruction it replaces (a `call`
before NOPing it, a `je` before NOPing it) and refuses on a mismatch. An
unknown build gets no patches at all, with a warning.

The current target, **2.748.0.1527**, boots through `StartAppWithParams` with no
image patches: the telemetry recursion below does not occur in it.

### Telemetry stop-gap (2.734.0.917 only)

The engine's eight `TelemetryBridge` JNI natives consult a per-thread
task/telemetry context (`0x2de3c50` reads `ctx2+0x28`) that the engine's
scheduler creates lazily. Until that scheduler runs, the natives take their
"context absent" path, dispatch through the object-table slot they themselves
occupy, and recurse until the guest stack is exhausted.

`RBXRT_PATCH_REG` controls the documented stop-gap applied before any guest code
runs (constructors included — their worker threads reach telemetry too):

- unset/`global` (default) — force `0x2de3c50` to report a context. `JNI_OnLoad`
  completes; the engine's thread telemetry registration is bypassed.
- `family` — NOP only the eight per-wrapper branches. Insufficient on its own:
  the registration path it leaves enabled still aborts.
- `off` — no patch; reproduces the recursion and the resulting crash.

Every enabled variant prints a warning: this is a stop-gap, not the engine's
natural behaviour, and it must be removed once the scheduler is up.
- [ ] `internal/fetch` — APKPure mirror download with digest verification.
- [ ] CLI (`run`/`doctor`; `verify`/`inspect`/`engine`/`load` exist).

## Debugging

The runtime is quiet by default; each diagnostic below is opt-in.

```
RBXRT_DEBUG_MEM=1    # guest stack, guard state, arena, map, exit-time frames
RBXRT_DEBUG_JNI=1    # per-call JNI entry trace (capped at 300 calls)
RBXRT_DEBUG_TLS=1    # per-call pthread_getspecific / guest-stack depth trace
RBXRT_DEBUG_LOOPER=1 # ALooper prepare/addFd/poll trace
RBXRT_DEBUG_ASSET=1  # AAssetManager open/lookup and openFileDescriptor trace
RBXRT_DEBUG_CONFIG=1 # AConfiguration synthesis trace
RBXRT_DEBUG_GFX=1    # X11 window, EGL display and window-surface mapping trace
RBXRT_DEBUG_NET=1    # getaddrinfo and socket traffic (=full dumps whole transfers)
RBXRT_DEBUG_FS=1     # open/fopen/stat/lstat/access by path, and fread per fd, with results
RBXRT_WALK=1         # dump the guest RBP chain at a fatal exit/abort
RBXRT_PATCH_REG=off  # disable the telemetry stop-gap to reproduce the crash (2.734)
RBXRT_GOMAXPROCS=<n> # Go P pool size (default 256; see cmd/rbxrt/main.go)
RBXRT_BREAK=<offs>   # int3 at these image offsets (hex, comma-separated), for gdb
```

The engine writes its own log to `<data>/appData/logs/*_last.log`. Its FLog
channels are fast flags, passed in through the client settings:

```
RBXRT_CLIENT_SETTINGS='{"FLogGraphics":"6","FLogSurfaceController":"6"}'
```

Shader work: `MESA_SHADER_DUMP_PATH=<dir>` (a Mesa variable) dumps every
shader the engine compiles; `RBXRT_GFX_TEST=1 RBXRT_GLSL_CORPUS=<dir> go test
./internal/hostabi -run TestGLSLRelaxCorpus` then checks them all against the
host driver. `RBXRT_BREAK` needs gdb to launch rbxrt itself (Yama
`ptrace_scope` 1 forbids attaching).

Boot inputs (`--launch`):

```
RBXRT_BOOT=1                # play the Java side: flags, settings, app bridge
RBXRT_DATA=<dir>            # app data directory; becomes the engine's cwd
RBXRT_BOOT_WATCHDOG=<secs>  # report stuck guest threads after this long (30)
```

With `RBXRT_BOOT` the data directory is prepared from the APK (`assets/content`,
`assets/android`, `assets/ExtraContent`, and `exe/cacert.pem` from the APK's
`assets/ssl/cacert.pem`) and becomes the working directory before any guest code
runs, so the engine's `cache/`, `http/` and `appData/` stay out of the directory
rbxrt was started from.

Graphics inputs:

```
RBXRT_GFX=off               # disable the graphics layer even for --launch
RBXRT_GFX=x11               # force the graphics layer on (e.g. with --entry)
RBXRT_WINDOW_WIDTH=<n>      # window width in pixels (default 1280)
RBXRT_WINDOW_HEIGHT=<n>     # window height in pixels (default 720)
```

Asset and device inputs for the app-entry probe:

```
RBXRT_APK=<path>            # APK whose assets/ back AAssetManager (e.g. roblox.apk)
RBXRT_ASSETS=<dir>          # alternatively, a directory of loose assets
RBXRT_SCREEN_WIDTH_DP=<n>   # logical width reported by AConfiguration (default 960)
RBXRT_SCREEN_HEIGHT_DP=<n>  # logical height reported by AConfiguration (default 540)
```

Language and country are derived from `LC_ALL`/`LANG` (e.g. `en_US.UTF-8` →
`en`/`US`).

## Development

```
go test ./...            # all tests
./scripts/fixtures.sh    # regenerate internal/apk/testdata with Android SDK tools
```

JNI/ART framework calls that we cannot honour are designed to fail loudly
rather than fake success; native code paths that cannot work on bare Linux are
shimmed (or refused) until implemented.

> **Legal note:** Roblox Corporation prohibits third-party clients. Requesting,
> downloading, or running Roblox through this runtime may breach Roblox's Terms
> of Service and result in account termination. Use only on accounts you are
> willing to lose.