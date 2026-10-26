# Roblox runtime milestones

Goal: real Roblox engine (`libroblox.so`, x86_64) boots in the native runtime,
initializes Mesa/zink graphics, loads a place, and renders frames to a real window.

Legend: `[x]` done · `[~]` in progress · `[ ]` not reached

- [x] 1. Load and relocate the stripped `libroblox.so` (120 MB), run its 3678 constructors
- [x] 2. JNI fake-object layer online: `JNI_OnLoad`, `RegisterNatives`, FindClass/fields/methods answered
- [x] 3. `GameActivity` lifecycle driven: onStart → onResume → focus → insets → surface created/changed/redraw
- [x] 4. `initializeNativeCode` returns a usable native handle
- [x] 5. Java boot data seeded before `JNI_OnLoad` (activity, resources, display metrics, InitParams, StartAppParams)
- [x] 6. `nativeGameGlobalInit()` succeeds
- [x] 7. `nativeAppBridgeV2InitWithParams(InitParams)` completes
- [ ] 8. `nativeAppBridgeV2StartAppWithParams(StartAppParams)` reaches the graphics path
- [ ] 9. EGL initialized on host Mesa: `eglInitialize` / `eglCreateContext` / `eglMakeCurrent` counted > 0
- [ ] 10. Render backend comes up (Mesa zink → Vulkan) and creates a swapchain/surface
- [ ] 11. Engine loads `rbxasset://places/Mobile.rbxl` (place data accepted)
- [ ] 12. First `eglSwapBuffers` with real content
- [ ] 13. Roblox window launches and shows a rendered frame

Current focus: milestone 8 — drive `nativeAppBridgeV2StartAppWithParams` into the
graphics path. Flags initialize, the client-settings sequence completes, and
`nativeAppBridgeV2InitWithParams` returns normally. Required filesystem paths
(`assets/content`, `assets/android`, `ExtraContent`, and `StudioContent`) are
prepared from the APK or created in the runtime data directory. The StartApp
path is still blocked: some runs trap in a telemetry assertion; others time out
inside initialization. EGL entry points have not been confirmed. The UniversalApp
patch configuration and guest-thread/TLS behavior remain unresolved.

Last updated: 2026-10-24
