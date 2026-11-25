# Roblox runtime milestones

Goal: real Roblox engine (`libroblox.so`, x86_64) boots in the native runtime,
initializes Mesa/zink graphics, loads a place, and renders frames to a real window.

Legend: `[x]` done · `[~]` in progress · `[ ]` not reached

Engine build: **Roblox 2.748.0.1527** (x86_64, from the universal APK; signature
verified with `rbxrt verify`). Milestones 1-7 were first reached on 2.734.0.917;
all of them hold on 2.748 too. Image offsets the runtime patches are kept per
build in `cmd/rbxrt/profile.go`.

- [x] 1. Load and relocate the stripped `libroblox.so` (118 MB), run its constructors (2355 on 2.748; 3678 on 2.734)
- [x] 2. JNI fake-object layer online: `JNI_OnLoad`, `RegisterNatives`, FindClass/fields/methods answered
- [x] 3. `GameActivity` lifecycle driven: onStart → onResume → focus → insets → surface created/changed/redraw
- [x] 4. `initializeNativeCode` returns a usable native handle
- [x] 5. Java boot data seeded before `JNI_OnLoad` (activity, resources, display metrics, InitParams, StartAppParams)
- [x] 6. `nativeGameGlobalInit()` succeeds
- [x] 7. `nativeAppBridgeV2InitWithParams(InitParams)` completes
- [x] 8. `nativeAppBridgeV2StartAppWithParams(StartAppParams)` reaches the graphics path
- [x] 9. EGL initialized on host Mesa: `eglInitialize` / `eglCreateContext` / `eglMakeCurrent` counted > 0
- [x] 10. Render backend comes up (Mesa zink → Vulkan) and creates a swapchain/surface
- [x] 11. Engine loads `rbxasset://places/Mobile.rbxl` (place data accepted)
- [~] 12. First `eglSwapBuffers` with real content
- [ ] 13. Roblox window launches and shows a rendered frame

## Milestone 8 (and 9)

`nativeAppBridgeV2StartAppWithParams` now returns on every run (10/10 clean
launches). On the way it loads and verifies the UniversalApp core-script patch,
creates the DataModel, initializes FMOD, and its SurfaceController creates the
render view: `eglGetDisplay` → `eglInitialize` → `eglCreateContext` →
`eglCreateWindowSurface` (the X11 window) → `eglMakeCurrent` on the host's Mesa.
The rest of the boot handshake (`initMaquettesSDK`, `StartLuaAppDM`,
`SendAppEventOnAppReady`, `ScheduleOnFirstFrame`, surface update) completes and
the engine reports `gameLoadedCallback`. The hold ends with
`EGL counts: initialize=1 context=1 makeCurrent=2 swap=0`.

```
RBXRT_APK=roblox.apk RBXRT_DATA=/path/to/data RBXRT_BOOT=1 \
  rbxrt load lib/x86_64/libroblox.so --launch
```

What it took (runtime fixes, not engine patches — 2.748 needs no image patches):

- **Build profiles.** Offsets are keyed by the engine's embedded version string;
  patches check the bytes they replace. 2.748 needs neither the telemetry
  stop-gap nor any of the 2.734 patches.
- **Boot order.** The native flag provider is initialized before
  `nativeGameGlobalInit`, as `Application.onCreate` does on Android.
- **Libc semantics.** Raw-syscall wrappers returned `-errno`; they now return
  `-1` and set a per-thread `errno` (curl read `connect`'s `-115` as success).
  `syscall()` follows the same convention, so timed futex waits see
  `ETIMEDOUT`.
- **Resolver.** `getaddrinfo` resolves hostnames (Go's resolver, off the guest
  stack), honours hints and writes correct `sockaddr`s with bionic's `EAI_*`
  codes; `inet_ntop` no longer panics.
- **Scheduler.** `GOMAXPROCS` is sized for the engine's threads: a guest thread
  blocked in a raw futex keeps its P, and four of them starved the process.
- **Fibers.** The engine runs tasks on its own mmap'd stacks. Host calls from a
  fiber (thunks, variadic and JNI trampolines) now move to a reserved area of
  the thread's guest stack instead of running Go on a stack the Go runtime does
  not know.
- **Threads.** Guest threads are locked to their OS threads: Mesa's current EGL
  context is per OS thread, and a migrating goroutine got `glGetString() == NULL`.
- **pthread mutexes.** Recursive and errorcheck types (bionic encoding), futex
  waits instead of spinning, and condition waits that release a recursively held
  mutex fully. FMOD deadlocked on its recursive mutexes before.
- **`setjmp`/`longjmp`** as raw machine code. Unimplemented-symbol traps now
  exit the process (`exit_group`) rather than silently ending one thread.
- **Data directory.** The engine's working directory is the data directory
  (`cache/`, `http/`, `appData/`, `exe/` live there, not in the repo);
  `ExtraContent` is staged under `assets/` where the engine looks for it (the
  core-script patch `UniversalApp.rbxm` resolves from there); the APK's
  `assets/ssl/cacert.pem` is staged as `exe/cacert.pem`.

## Milestone 10

The engine's GLES renderer comes up on the host's Mesa, which on this machine
is **zink on Vulkan** (`GL Renderer: zink Vulkan 1.4 (Intel UHD Graphics (JSL))`,
`OpenGL ES 3.2 Mesa 26.3.1`). The device reports its caps, all 657 shaders of
the `glsles3` pack compile, the SurfaceController's render job runs, and
frames are presented through zink's Vulkan swapchain: about 515
`eglSwapBuffers` per 15 s hold (~33 fps), 5/5 clean runs. The engine's own
Vulkan mode is not used (it declines: "Android version is too old").

What it took:

- **Engine logs.** `vsnprintf` returned empty strings during a boot (an old
  stack-safety shortcut), so every engine-formatted string was empty - log
  file names included. Fixed; the engine now writes
  `appData/logs/<version>_<time>_Player_<id>_last.log`, and FLog channels can
  be enabled with `RBXRT_CLIENT_SETTINGS='{"FLogGraphics":"6"}'`.
- **Shader pack.** `assets/shaders/` (and `assets/fonts/`) are staged; the GLES
  renderer refuses its graphics mode without `shaders_glsles3.pack`.
- **`sscanf`.** Integer conversions consumed the character after the number
  (a `break` that only left a `switch`), read the first character twice, and
  negated negative numbers twice; float conversions read the first character
  twice. The renderer parses every shader's uniform blocks with
  `sscanf("uniform vec4 CB%d [%d];")`, so all 1590 shader creations threw.
- **GLSL ES leniency.** Terrain shaders multiply `uint * int`, which mobile
  drivers accept and GLSL ES 3.00 (Mesa) does not. `glShaderSource` resubmits
  ES 3.00 shaders as ES 3.10 with `GL_EXT_shader_implicit_conversions`;
  validated on Mesa against the whole pack (`TestGLSLRelaxCorpus`).

## Milestone 11

The app shell loads its place from the bundled asset: the engine logs
`DataModel Loading rbxasset://places/Mobile.rbxl`, resolves it to
`assets/ExtraContent/places/Mobile.rbxl` (after probing `assets/android/`),
reads all 147,973 bytes in one `fread`, initializes the place's services and
reports `onGameLoaded` about a second later; the Lua app then reaches its
`Landing` stage. No runtime change was needed beyond milestone 10's fixes (the
ExtraContent location and working `vsnprintf`/`sscanf`). `RBXRT_DEBUG_FS` now
also logs each `fread` per descriptor, which is how the byte count above was
checked.

## Milestone 12 status

Frames are swapped continuously, and their content is the engine's own clear
colour (RGB 11,11,11 over the window's black background), not UI yet. The Lua
app reaches its `Landing` stage, but its screens need network data (HTTPS is
still failing, below) and nothing is drawn over the clear.

## Known open issues

- **TLS handshakes fail.** Connections to `apis.roblox.com` get as far as the
  server's TLS 1.3 ServerHello (x25519, AES-256-GCM), then the engine's OpenSSL
  answers `illegal_parameter` and closes. The hellos are well-formed, so the
  failure is inside OpenSSL's key-share processing on this runtime. Boot does
  not depend on it yet (flags come from the local flag cache and the bundled
  patch is current), but anything online will.

Current focus: milestone 12 — get UI on screen (the Lua app needs network data: fix TLS).

Last updated: 2026-11-25
