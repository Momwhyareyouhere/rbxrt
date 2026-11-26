package hostabi

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The graphics platform layer.
//
// Android's platform provides EGL, GLES and the window system; on the host those
// are Mesa's libEGL, libGLESv2 and a native window: a Wayland xdg_toplevel
// (gfx_wayland.go) or an X11 window (gfx_x11.go). The runtime does not reimplement a GPU
// driver. Instead it loads the host's real libraries (see hostdl.go) and binds
// the engine's imports to them, so the engine drives the same Mesa driver a
// native Linux program would. Two entry points cannot pass straight through
// because the guest speaks Android's shape, not the host's:
//
//   - eglGetDisplay: the engine asks for EGL_DEFAULT_DISPLAY; we hand back the
//     platform display (EGL_PLATFORM_WAYLAND or EGL_PLATFORM_X11) for the
//     window we opened, so the platform always matches the window.
//   - eglCreateWindowSurface: the engine passes an ANativeWindow; Mesa wants a
//     wl_egl_window* or an X11 Window (XID). We translate between the two.
//
// The window system is chosen by RBXRT_DISPLAY: "wayland", "x11", or "auto"
// (the default: native Wayland when WAYLAND_DISPLAY is set, falling back to
// X11/Xwayland if that cannot start).
//
// Everything else — the other egl* calls and every gl* call — is bound directly
// to Mesa, which is also why the engine's floating-point and wider argument
// lists work at all: no thunk sits between the guest and the driver.

var debugGFX = os.Getenv("RBXRT_DEBUG_GFX") != ""

// EGL lifecycle call counters. The launch driver reads these to answer the one
// question it needs about rendering: did the engine ever reach EGL, and if so
// how far down the path (display -> context -> make-current -> swap) did it
// get? Only these calls are counted; every gl* call stays bound directly to
// Mesa so a real frame costs nothing extra.
var (
	eglInitCount        int64
	eglContextCount     int64
	eglMakeCurrentCount int64
	eglSwapCount        int64
)

// EGLCounts returns the number of times each EGL lifecycle call has been made.
func EGLCounts() (inits, contexts, makeCurrents, swaps int64) {
	return atomic.LoadInt64(&eglInitCount), atomic.LoadInt64(&eglContextCount),
		atomic.LoadInt64(&eglMakeCurrentCount), atomic.LoadInt64(&eglSwapCount)
}

const windowFormatRGBA8888 = 1

// hostWindow describes one ANativeWindow handed to the engine. It records which
// native window (wl_egl_window* or XID) backs it so eglCreateWindowSurface can
// translate.
type hostWindow struct {
	native uintptr
	w, h   int
	format int
}

type gfxState struct {
	ready   bool
	backend string

	egl, gles uintptr

	platform      string  // "wayland" or "x11"
	eglPlatform   uintptr // EGL_PLATFORM_* for eglGetPlatformDisplayEXT
	nativeDisplay uintptr // wl_display* or Display*
	nativeWindow  uintptr // wl_egl_window* or Window
	width         int
	height        int

	fnEglGetDisplay          uintptr
	fnEglGetPlatformDisplay  uintptr
	fnEglCreateWindowSurface uintptr
	fnEglInitialize          uintptr
	fnEglCreateContext       uintptr
	fnEglMakeCurrent         uintptr
	fnEglSwapBuffers         uintptr

	eglDisplay uintptr

	mu      sync.Mutex
	windows map[uintptr]*hostWindow
	nextWin uintptr
}

var gfx gfxState

// graphicsBackend selects the Mesa driver that will execute the engine's GL.
// The engine imports EGL + GLES2 only - it has no Vulkan entry points of its
// own - so its Vulkan rendering path is Mesa's zink driver, which turns those
// GL calls into Vulkan. Verified on this host: "OpenGL renderer string: zink
// Vulkan 1.4 (Intel(R) UHD Graphics)". RBXRT_GFX_BACKEND=mesa picks Mesa's own
// GL driver instead; an explicit MESA_LOADER_DRIVER_OVERRIDE from the caller's
// environment always wins, so the value has to be set before libEGL is loaded.
func graphicsBackend() string {
	if o := os.Getenv("MESA_LOADER_DRIVER_OVERRIDE"); o != "" {
		return "override:" + o
	}
	if strings.EqualFold(os.Getenv("RBXRT_GFX_BACKEND"), "mesa") {
		return "mesa"
	}
	os.Setenv("MESA_LOADER_DRIVER_OVERRIDE", "zink")
	return "zink"
}

// GraphicsEnabled reports whether the graphics layer is live.
func GraphicsEnabled() bool { return gfx.ready }

// WindowSize returns the graphics window size in pixels (0,0 before
// InitGraphics succeeds).
func WindowSize() (int, int) { return gfx.width, gfx.height }

// DisplayPlatform names the window system in use ("wayland" or "x11"; empty
// before InitGraphics succeeds).
func DisplayPlatform() string { return gfx.platform }

// InitGraphics binds the engine's graphics imports to the host's real EGL/GLES
// implementation and opens one native window for it to render into. It is
// idempotent. On failure nothing is registered and the engine will still trap,
// loudly, at the first graphics call — the runtime never pretends to render.
func InitGraphics() error {
	if gfx.ready {
		return nil
	}
	// Select the driver before libEGL is loaded: Mesa reads the override when
	// the DRI driver is opened.
	gfx.backend = graphicsBackend()
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no DISPLAY/WAYLAND_DISPLAY; nothing to render into")
	}
	if !hostDlAvailable() {
		return fmt.Errorf("host dynamic linker (dlopen) not found: rbxrt must be dynamically linked")
	}

	w, h := windowSize()
	if err := openWindow(w, h); err != nil {
		return err
	}
	gfx.width, gfx.height = w, h

	egl := hostDlopen("libEGL.so.1", rtldNow|rtldGlobal)
	if egl == 0 {
		return fmt.Errorf("cannot dlopen libEGL.so.1")
	}
	gles := hostDlopen("libGLESv2.so.2", rtldNow|rtldGlobal)
	if gles == 0 {
		return fmt.Errorf("cannot dlopen libGLESv2.so.2")
	}
	gfx.egl, gfx.gles = egl, gles

	// Mesa entry points the runtime wraps.
	gfx.fnEglGetDisplay = hostDlsym(egl, "eglGetDisplay")
	gfx.fnEglGetPlatformDisplay = hostDlsym(egl, "eglGetPlatformDisplayEXT")
	gfx.fnEglCreateWindowSurface = hostDlsym(egl, "eglCreateWindowSurface")
	gfx.fnEglInitialize = hostDlsym(egl, "eglInitialize")
	gfx.fnEglCreateContext = hostDlsym(egl, "eglCreateContext")
	gfx.fnEglMakeCurrent = hostDlsym(egl, "eglMakeCurrent")
	gfx.fnEglSwapBuffers = hostDlsym(egl, "eglSwapBuffers")
	if gfx.fnEglCreateWindowSurface == 0 || gfx.fnEglInitialize == 0 ||
		gfx.fnEglCreateContext == 0 || gfx.fnEglMakeCurrent == 0 || gfx.fnEglSwapBuffers == 0 {
		return fmt.Errorf("libEGL is missing eglCreateWindowSurface/eglInitialize/eglCreateContext/eglMakeCurrent/eglSwapBuffers")
	}

	// The wrapped entry points and the ANativeWindow surface API must resolve
	// to Go implementations (hostFuncs takes precedence over the raw resolver).
	hostFuncs["eglGetDisplay"] = goEglGetDisplay
	hostFuncs["eglCreateWindowSurface"] = goEglCreateWindowSurface
	hostFuncs["eglInitialize"] = goEglInitialize
	hostFuncs["eglCreateContext"] = goEglCreateContext
	hostFuncs["eglMakeCurrent"] = goEglMakeCurrent
	// Under RBXRT_DEBUG_GFX, count the draw, blit and framebuffer calls the
	// engine makes - imported or looked up at run time - and report them every
	// 60 swaps (gfxcount.go).
	if debugGFX {
		if eglGetProcAddressHost = hostDlsym(gfx.egl, "eglGetProcAddress"); eglGetProcAddressHost != 0 {
			hostFuncs["eglGetProcAddress"] = goEglGetProcAddress
		}
	}
	// GLSL ES compatibility shim; see glsl.go.
	if glShaderSourceHost = hostDlsym(gfx.gles, "glShaderSource"); glShaderSourceHost != 0 {
		hostFuncs["glShaderSource"] = goGlShaderSource
	}
	hostFuncs["eglSwapBuffers"] = goEglSwapBuffers
	hostFuncs["ANativeWindow_fromSurface"] = goANativeWindowFromSurface
	hostFuncs["ANativeWindow_getWidth"] = goANativeWindowGetWidth
	hostFuncs["ANativeWindow_getHeight"] = goANativeWindowGetHeight
	hostFuncs["ANativeWindow_acquire"] = goANativeWindowAcquire
	hostFuncs["ANativeWindow_release"] = goANativeWindowRelease

	gfx.windows = map[uintptr]*hostWindow{}
	gfx.nextWin = 0x7b200000

	rawResolver = resolveGPU
	gfx.ready = true

	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: %s window %#x %dx%d, libEGL/libGLESv2 loaded\n",
			gfx.platform, gfx.nativeWindow, w, h)
	}
	return nil
}

// openWindow opens the native window selected by RBXRT_DISPLAY.
func openWindow(w, h int) error {
	hasWayland := os.Getenv("WAYLAND_DISPLAY") != ""
	hasX11 := os.Getenv("DISPLAY") != ""
	switch mode := strings.ToLower(os.Getenv("RBXRT_DISPLAY")); mode {
	case "wayland":
		return openWayland(w, h)
	case "x11":
		return openX11(w, h)
	case "", "auto":
		if hasWayland {
			err := openWayland(w, h)
			if err == nil || !hasX11 {
				return err
			}
			fmt.Fprintf(os.Stderr, "rbxrt: graphics: native Wayland unavailable (%v); using X11 (Xwayland)\n", err)
		}
		return openX11(w, h)
	default:
		return fmt.Errorf("RBXRT_DISPLAY=%q: want wayland, x11 or auto", mode)
	}
}

func windowSize() (int, int) {
	return envInt("RBXRT_WINDOW_WIDTH", 1280), envInt("RBXRT_WINDOW_HEIGHT", 720)
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// resolveGPU is the last-chance symbol oracle: it binds an unknown gl*/egl*
// import to the host's real implementation. Only names that look like GL/EGL
// entry points are considered, so unrelated "gl"-prefixed symbols are not
// silently captured.
func resolveGPU(name string) (uintptr, bool) {
	if !gfx.ready {
		return 0, false
	}
	var a uintptr
	switch {
	case looksLikeEGL(name):
		a = hostDlsym(gfx.egl, name)
		if a == 0 {
			a = hostDlsym(gfx.gles, name)
		}
	case looksLikeGL(name):
		a = hostDlsym(gfx.gles, name)
		if a == 0 {
			a = hostDlsym(gfx.egl, name)
		}
	default:
		return 0, false
	}
	if debugGFX {
		a = countedGL(name, a)
	}
	return a, a != 0
}

func looksLikeGL(name string) bool {
	return len(name) >= 3 && name[:2] == "gl" && isUpperOrDigit(name[2])
}

func looksLikeEGL(name string) bool {
	return len(name) >= 4 && name[:3] == "egl" && isUpperOrDigit(name[3])
}

func isUpperOrDigit(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// --- wrapped EGL entry points -------------------------------------------

//go:noinline
func goEglGetDisplay(nativeDisplay uintptr) uintptr {
	if gfx.eglDisplay != 0 {
		return gfx.eglDisplay
	}
	var d uintptr
	if gfx.fnEglGetPlatformDisplay != 0 {
		d = callHostC(gfx.fnEglGetPlatformDisplay,
			&[9]uintptr{gfx.eglPlatform, gfx.nativeDisplay, 0})
	}
	if d == 0 && gfx.fnEglGetDisplay != 0 {
		d = callHostC(gfx.fnEglGetDisplay, &[9]uintptr{gfx.nativeDisplay})
	}
	gfx.eglDisplay = d
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglGetDisplay(guest=%#x) -> %s platform display %#x\n",
			nativeDisplay, gfx.platform, d)
	}
	return d
}

//go:noinline
func goEglCreateWindowSurface(display, config, nativeWindow, attribs uintptr) uintptr {
	native := nativeWindow
	gfx.mu.Lock()
	if w, ok := gfx.windows[nativeWindow]; ok {
		native = w.native
	}
	gfx.mu.Unlock()
	r := callHostC(gfx.fnEglCreateWindowSurface, &[9]uintptr{display, config, native, attribs})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglCreateWindowSurface(anw=%#x) %s window=%#x -> %#x\n",
			nativeWindow, gfx.platform, native, r)
	}
	return r
}

// The remaining EGL lifecycle calls are wrapped only to count them (and, under
// RBXRT_DEBUG_GFX, to log the first few). The calls themselves pass straight
// through: these four are cold apart from eglSwapBuffers, which is one counter
// increment per frame.

//go:noinline
func goEglInitialize(display, major, minor uintptr) uintptr {
	atomic.AddInt64(&eglInitCount, 1)
	r := callHostC(gfx.fnEglInitialize, &[9]uintptr{display, major, minor})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglInitialize(%#x) -> %#x (count %d)\n",
			display, r, atomic.LoadInt64(&eglInitCount))
	}
	return r
}

//go:noinline
func goEglCreateContext(display, config, share, attribs uintptr) uintptr {
	atomic.AddInt64(&eglContextCount, 1)
	r := callHostC(gfx.fnEglCreateContext, &[9]uintptr{display, config, share, attribs})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglCreateContext(dpy=%#x cfg=%#x share=%#x) -> %#x (count %d)\n",
			display, config, share, r, atomic.LoadInt64(&eglContextCount))
	}
	return r
}

//go:noinline
func goEglMakeCurrent(display, draw, read, ctx uintptr) uintptr {
	atomic.AddInt64(&eglMakeCurrentCount, 1)
	r := callHostC(gfx.fnEglMakeCurrent, &[9]uintptr{display, draw, read, ctx})
	if debugGFX {
		rp, _ := guestCapture()
		writeStderrRaw(fmt.Sprintf("rbxrt: gfx: eglMakeCurrent(dpy=%#x draw=%#x read=%#x ctx=%#x) -> %#x (count %d) thread=%#x caller=%s\n",
			display, draw, read, ctx, r, atomic.LoadInt64(&eglMakeCurrentCount), getg(), guestAddr(rp)))
	}
	return r
}

//go:noinline
func goEglSwapBuffers(display, surface uintptr) uintptr {
	if n := atomic.AddInt64(&eglSwapCount, 1); debugGFX && n%60 == 0 {
		writeStderrRaw(fmt.Sprintf("rbxrt: gfx: swap %d: last 60 frames: %s\n", n, glCallCounts()))
	}
	if gfx.platform == "wayland" {
		waylandPump()
	}
	r := callHostC(gfx.fnEglSwapBuffers, &[9]uintptr{display, surface})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglSwapBuffers(dpy=%#x surf=%#x) -> %#x (count %d)\n",
			display, surface, r, atomic.LoadInt64(&eglSwapCount))
	}
	return r
}

// BindGuestSurface publishes a guest-side stand-in for android.view.Surface as
// an ANativeWindow backed by this runtime's native window. The launch driver hands
// the engine a synthetic Surface handle in its lifecycle callbacks; if the
// engine passes that handle straight to eglCreateWindowSurface instead of going
// through ANativeWindow_fromSurface first, the wrapped create-window-surface
// must still find the real native window behind it. Handles the driver never
// registers keep their old behaviour (passed to Mesa verbatim).
func BindGuestSurface(surface uintptr) {
	if surface == 0 {
		return
	}
	gfx.mu.Lock()
	defer gfx.mu.Unlock()
	if _, ok := gfx.windows[surface]; ok {
		return
	}
	w, h := gfx.width, gfx.height
	if w == 0 {
		w = 1280
	}
	if h == 0 {
		h = 720
	}
	gfx.windows[surface] = &hostWindow{native: gfx.nativeWindow, w: w, h: h, format: windowFormatRGBA8888}
	if debugGFX {
		fmt.Printf("rbxrt: gfx: guest surface %#x bound to %s window %#x (%dx%d)\n",
			surface, gfx.platform, gfx.nativeWindow, w, h)
	}
}

func goANativeWindowFromSurface(env, surface uintptr) uintptr {
	gfx.mu.Lock()
	defer gfx.mu.Unlock()
	p := gfx.nextWin
	gfx.nextWin += 16
	gfx.windows[p] = &hostWindow{
		native: gfx.nativeWindow,
		w:      gfx.width,
		h:      gfx.height,
		format: windowFormatRGBA8888,
	}
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: ANativeWindow_fromSurface(surface=%#x) -> %#x\n",
			surface, p)
	}
	return p
}

//go:nosplit
func goANativeWindowGetWidth(win uintptr) int {
	if gfx.width != 0 {
		return gfx.width
	}
	return 1280
}

//go:nosplit
func goANativeWindowGetHeight(win uintptr) int {
	if gfx.height != 0 {
		return gfx.height
	}
	return 720
}

func goANativeWindowAcquire(win uintptr) {}

func goANativeWindowRelease(win uintptr) {}

var eglGetProcAddressHost uintptr

// goEglGetProcAddress (RBXRT_DEBUG_GFX only) logs each GL/EGL entry point the
// engine looks up at run time, and hands out the counting draw wrappers for
// for the counted calls a counting trampoline (gfxcount.go).
//
//go:noinline
func goEglGetProcAddress(name uintptr) uintptr {
	n := cstr(name)
	writeStderrRaw("rbxrt: gfx: eglGetProcAddress(" + n + ")\n")
	return countedGL(n, callHostC(eglGetProcAddressHost, &[9]uintptr{name}))
}
