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
- [~] 12. First `eglSwapBuffers` with real content (frames presented; no UI drawn yet)
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

## Milestone 12 status (in progress)

With the engine's built-in flag defaults, frames are presented continuously
(~600 per 15 s hold) but carry only the clear colour (RGB 11,11,11): the
logged-out app shell sends `ACTION_LOG_IN` to the platform, i.e. asks Android
for its native Java login screen, and draws nothing itself.

The live Android settings (25,662 flags from
`clientsettingscdn.roblox.com/v2/settings/application/AndroidApp`, fetched with
`RBXRT_FETCH_SETTINGS=1`) set `FIntLuaAppLoginMethod=1`, under which the Lua app
draws its own login screen - real UI. They also switch the engine to its newer
startup path, which does not complete here yet:

- `FFlagSlowDownRendering` starts the render job throttled. On a device the
  throttle is released by performance control's `AppStartRendering` tunable or
  by a 10 s safety timer (`RenderThrottleTimeout`), which runs on the task
  scheduler's `TimerTickerJob`. Neither fires in this runtime: the timer is
  armed (seen under gdb) but its callback never runs.
- With the throttle overridden off, the render job steps but reports
  `stepDataModelJob: No DM yet`: the Lua app's DataModel loads (patch verified,
  `Mobile.rbxl` read) but is never handed to the render job.

Root causes found since (flag bisection with `RBXRT_CLIENT_SETTINGS_FILE`):

- **`FFlagVideoCaptureUseNewService6`** makes `ViewRbx::bindWorkspace` wait on
  the new video-capture service, which on Android sits on MediaCodec/AImageReader
  (not provided by this runtime). The render job waits for that bind
  (`scheduleRender`), the task scheduler's frame never ends, and with it the
  `TimerTickerJob` that would release the startup throttle - the "timer never
  fires" above was a consequence.
- **`DFFlagUseVisBugChecks`** divides by a zero viewport aspect in software
  occlusion setup (integer divide fault).
- **Storage directories.** RbxStorage needs the app's cache directory; the boot
  now passes files/cache/external directories (`nativeSetFilesDirectory`,
  `nativeSetCacheDirectory`, `nativeSetExternalDirectory`,
  `nativeSetBaseDataDirectories`), which fixed an `RBXCRASH` ("Path does not
  exist").
- With those two flags and `FFlagSlowDownRendering` off, the live settings run
  cleanly and present ~700 frames per hold. With `FFlagSlowDownRendering` on,
  the engine still crashes intermittently (varying sites) - not yet understood.
- The boot no longer calls `nativeAppBridgeStartLuaAppDM` ("initialize with the
  logged-in user") unless `RBXRT_LUA_APP_LOGGED_IN=1`: the runtime has no
  session, and claiming one sent the app to a logged-in home whose requests all
  fail with 401.

Still open: under every configuration the frame carries ~7 draw calls and no
visible UI; the app reaches `Landing` but its 2D UI is not drawn. Next: find
why the GUI layer renders nothing.

Runtime fixes made on the way (all with tests):

- **TLS works.** `qsort`/`bsearch` read the guest comparator's full `%rax`; a
  comparator returning `-1` in `%eax` looked positive, so OpenSSL's cipher
  table was never sorted and the server's chosen suite was "unknown"
  (`set_client_ciphersuite`, found by breaking on `ERR_set_debug` under gdb).
  Every int-returning guest callback now sign-extends from 32 bits. HTTPS to
  Roblox now succeeds (e.g. `users.roblox.com` answers 401 "Authentication token
  is missing" for the logged-out app).
- **Signals.** `sigaction` wrote glibc's 152-byte struct into bionic's 32-byte
  one (curl's SIGPIPE save tripped the stack protector); `sigset_t` is 8 bytes
  on LP64 bionic, not 128. Actions are now recorded per signal.
- **`sscanf %zu`** (and `%j`, `%t`): the engine's memory sampler reads
  `/proc/self/statm` with it and failed every 100 ms.
- **Condition variables honour their clock.** `pthread_condattr_setclock` was
  ignored, so every `steady_clock` wait used a monotonic deadline as a realtime
  one and timed out at once; interrupted absolute waits now resume.
- **`nativePostClientSettingsLoadedInitialization3`** is called after the
  settings, as Java does (it applies them to the app-bridge components).
- JNI `ExceptionOccurred` answers "none".

## Known open issues

- The live-flags startup path (milestone 12 status above).
- Logging in needs a Roblox session; the runtime does not provide one.

Current focus: milestone 12 — complete the live-flags startup path so the Lua login screen draws.

Last updated: 2026-11-25
