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
- [x] 7. `nativeAppBridgeV2InitWithParams(InitParams)` returns normally
- [~] 8. `nativeAppBridgeV2StartAppWithParams(StartAppParams)` reaches the graphics path
      (currently SIGSEGVs: app-bridge components object at `+0x440` is null)
- [ ] 9. EGL initialized on host Mesa: `eglInitialize` / `eglCreateContext` / `eglMakeCurrent` counted > 0
- [ ] 10. Render backend comes up (Mesa zink → Vulkan) and creates a swapchain/surface
- [ ] 11. Engine loads `rbxasset://places/Mobile.rbxl` (place data accepted)
- [ ] 12. First `eglSwapBuffers` with real content
- [ ] 13. Roblox window launches and shows a rendered frame

Current focus: milestone 8 — find why the components/settings function at
`IMG+0x245efca` (called from `IMG+0x245e313` during `InitWithParams`) never reaches
its later stages, leaving `*(appObject+0x440)` null and crashing the callee at
`IMG+0x2715185`.

Last updated: 2026-10-23
