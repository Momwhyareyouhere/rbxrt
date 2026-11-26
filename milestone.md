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
- [x] 12. First `eglSwapBuffers` with real content
- [x] 13. Roblox window launches and shows a rendered frame

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

## Milestones 12 and 13

A default launch shows Roblox's real start screen in the X11 window: the
Roblox logo, "Create Account" and "Sign In" buttons, Terms/Privacy links, and
the game-tile backdrop, whose images are downloaded live over HTTPS. It is
drawn by the engine's GLES renderer on Mesa zink/Vulkan (~38 indexed draws a
frame) and is identical across 3/3 clean runs.

```
RBXRT_APK=base.apk RBXRT_DATA=<dir> RBXRT_BOOT=1 rbxrt load libroblox.so --launch
```

What it took, on top of milestone 11:

- **JNI field getters.** The dispatcher took every even JNI table index in
  95..112 (and 145..162) for a setter: `GetBooleanField`, `GetCharField`,
  `GetIntField` and `GetFloatField` all returned 0, and float results were
  never placed in `%xmm0`. The engine read `PlatformParams.dpiScale` as 0 and
  laid the whole UI out at zero size (no UI draw calls at all). The same zeros
  caused the `DFFlagUseVisBugChecks` divide-by-zero and the intermittent crashes
  with `FFlagSlowDownRendering` - both gone.
- **Live client settings by default.** The boot fetches the AndroidApp flags
  from `clientsettingscdn.roblox.com` (cached; `RBXRT_OFFLINE=1` skips the fetch).
  With them the Lua app draws its own start screen (`FIntLuaAppLoginMethod=1`);
  with the built-in defaults it waits for a native Java login screen.
  One runtime override remains: `FFlagVideoCaptureUseNewService6=False`, since
  the new video-capture service needs Android's MediaCodec/AImageReader, and
  binding the workspace waits on it forever (found by bisecting the flags with
  `RBXRT_CLIENT_SETTINGS_FILE`).
- **Device facts.** Android 14 (API 34) is reported through
  `__system_property_get` (`ro.build.version.sdk`, ...), `Build.VERSION`, and the
  `DeviceInfo` object passed to `nativeSetDeviceInfo` (whose `osVersion` the
  client parses as the API level). At API 0 the app showed "Roblox no longer
  supports Android 6 and 7".
- **Storage directories** (`nativeSetFilesDirectory`, `nativeSetCacheDirectory`,
  ...) and a **logged-out boot** (no `nativeAppBridgeStartLuaAppDM` unless
  `RBXRT_LUA_APP_LOGGED_IN=1`).

## Input

Mouse and keyboard reach the engine through Roblox's own Java-side natives,
found by reading the APK's dex (`uk.f` and `h` call them):

- `NativeInputInterface.nativePassMouseMove(x, y, dx, dy)`,
  `nativePassMouseButton(x, y, down, actionButton-1)`,
  `nativePassMouseWheel(x, y, vscroll)`, in dp (density 1 here).
- `NativeGLInterface.nativePassKeyEvent(down, scanCode, keyCode, repeat)`.
- Text boxes: the engine calls `NativeGLJavaInterface.showKeyboard(box, ...)` /
  `hideKeyboard()`. Each edit goes to `syncTextboxTextAndCursorPosition2(text,
  cursor)`. Enter goes to `nativeReturnPressedFromOnScreenKeyboard(box)` +
  `nativePassText(box, text, true, cursor)`; the engine ignores `nativePassText`
  when the flag is false.

GameActivity's `onTouchEventNative` accepts events but the app ignores them;
Roblox does not read input from the native-app-glue queue.

Keyboard layouts come from libxkbcommon (`internal/hostabi/xkb.go`): the
Wayland compositor's keymap or the X server's, with the locale's compose
table for dead keys. Letters and digits get the Android key code of the
key's label in that layout (German Z key = `KEYCODE_Z`), as Android's
KeyCharacterMap does.

Two JNI-layer fixes were needed: static `CallStatic*Method` calls decoded their
first argument as if they were the `A` (jvalue array) form, and `byte[]` was
not implemented, so every `String.getBytes("UTF-8")` (the engine's jstring →
std::string path) produced an empty string.

## Android NDK platform layer

The platform libraries the engine loads at run time are provided by the
runtime: `dlopen("libaaudio.so" | "libandroid.so" | "libmediandk.so")` returns a
runtime handle, and `dlsym` on it resolves only implemented functions (others
are NULL, as on an older Android release, never a trap).

- AAudio → PulseAudio/PipeWire (`internal/hostabi/aaudio.go`): FMOD's
  callback-mode output stream, 48 kHz stereo float or int16, 10 ms bursts,
  called on a dedicated guest thread and paced by `pa_simple_write`. FMOD's Java
  capability queries (`org.fmod.FMOD.supportsAAudio` etc.) are answered so it
  picks AAudio.
- Media (`media.go`): `AMediaFormat` store; codecs and image readers
  unavailable.
- Thermal (`thermal.go`): headroom = hottest zone / its critical trip point.
- libc networking and friends (`libc4.go`).

Only five `long double` math helpers are still bound to traps.

## Known open issues

- Text typed into a focused text box shows up only after the box loses focus:
  on Android the engine hides it while the native keyboard overlay would be
  drawing it.
- Password sign-in stops at Roblox's security challenge
  (`ChallengeHybridWebView`): there is no Android web view. Quick Sign-in
  works, and the session persists across restarts (session.go).
- At start-up the Lua app briefly assumes a signed-in user, gets 401 from
  `app-launch-info`, and polls `friends/v1/users/0/friends/online` until
  rate-limited (429) when nobody is signed in.
- The engine's own Vulkan renderer does not load ("Unable to load Vulkan API");
  it falls back to GLES, which runs on Vulkan through zink anyway.
- Audio is untested in the engine: FMOD opens its output only when a game
  runs, which needs a signed-in session. AAudio itself works on the host
  (`TestAAudioPlayback`). The microphone is not supported (input streams are
  refused).
- No hardware video decoding: `AMediaCodec` reports no codec, as a device
  without one does.

Current focus: signing in (a Roblox session), which gates games and audio.

Last updated: 2026-11-26
