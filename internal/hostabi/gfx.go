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
// are Mesa's libEGL, libGLESv2 and Xlib. The runtime does not reimplement a GPU
// driver. Instead it loads the host's real libraries (see hostdl.go) and binds
// the engine's imports to them, so the engine drives the same Mesa driver a
// native Linux program would. Two entry points cannot pass straight through
// because the guest speaks Android's shape, not the host's:
//
//   - eglGetDisplay: the engine asks for EGL_DEFAULT_DISPLAY; we hand back the
//     X11 platform display we opened, so the chosen platform is deterministic
//     even on a Wayland session (where Mesa's default could be Wayland, which
//     our X11 window is not a valid surface for).
//   - eglCreateWindowSurface: the engine passes an ANativeWindow; Mesa's X11
//     platform wants an X11 Window (XID). We translate between the two.
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

const (
	eglPlatformX11EXT    = 0x31D5
	windowFormatRGBA8888 = 1
)

// hostWindow describes one ANativeWindow handed to the engine. It records which
// X11 window backs it so eglCreateWindowSurface can translate.
type hostWindow struct {
	xid    uintptr
	w, h   int
	format int
}

type gfxState struct {
	ready   bool
	backend string

	xlib, egl, gles uintptr

	xDisplay uintptr
	xWindow  uintptr
	width    int
	height   int

	fnXOpenDisplay        uintptr
	fnXDefaultRootWindow  uintptr
	fnXCreateSimpleWindow uintptr
	fnXMapWindow          uintptr
	fnXStoreName          uintptr
	fnXFlush              uintptr

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

// InitGraphics binds the engine's graphics imports to the host's real EGL/GLES
// implementation and opens one X11 window for it to render into. It is
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

	xlib := hostDlopen("libX11.so.6", rtldNow|rtldGlobal)
	if xlib == 0 {
		return fmt.Errorf("cannot dlopen libX11.so.6")
	}
	egl := hostDlopen("libEGL.so.1", rtldNow|rtldGlobal)
	if egl == 0 {
		return fmt.Errorf("cannot dlopen libEGL.so.1")
	}
	gles := hostDlopen("libGLESv2.so.2", rtldNow|rtldGlobal)
	if gles == 0 {
		return fmt.Errorf("cannot dlopen libGLESv2.so.2")
	}
	gfx.xlib, gfx.egl, gfx.gles = xlib, egl, gles

	gfx.fnXOpenDisplay = hostDlsym(xlib, "XOpenDisplay")
	gfx.fnXDefaultRootWindow = hostDlsym(xlib, "XDefaultRootWindow")
	gfx.fnXCreateSimpleWindow = hostDlsym(xlib, "XCreateSimpleWindow")
	gfx.fnXMapWindow = hostDlsym(xlib, "XMapWindow")
	gfx.fnXStoreName = hostDlsym(xlib, "XStoreName")
	gfx.fnXFlush = hostDlsym(xlib, "XFlush")
	if gfx.fnXOpenDisplay == 0 || gfx.fnXCreateSimpleWindow == 0 || gfx.fnXMapWindow == 0 {
		return fmt.Errorf("libX11 is missing the window-creation entry points")
	}

	dpy := callHostC(gfx.fnXOpenDisplay, &[9]uintptr{0})
	if dpy == 0 {
		return fmt.Errorf("XOpenDisplay failed")
	}
	gfx.xDisplay = dpy
	root := callHostC(gfx.fnXDefaultRootWindow, &[9]uintptr{dpy})

	w, h := windowSize()
	win := callHostC(gfx.fnXCreateSimpleWindow, &[9]uintptr{
		dpy, root, 60, 60, uintptr(w), uintptr(h), 0, 0, 0})
	if win == 0 {
		return fmt.Errorf("XCreateSimpleWindow failed")
	}
	gfx.xWindow = win
	callHostC(gfx.fnXStoreName, &[9]uintptr{dpy, win, CString("rbxrt")})
	callHostC(gfx.fnXMapWindow, &[9]uintptr{dpy, win})
	callHostC(gfx.fnXFlush, &[9]uintptr{dpy})
	gfx.width, gfx.height = w, h

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
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: X11 window %#x %dx%d, libEGL/libGLESv2 loaded\n",
			win, w, h)
	}
	return nil
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
			&[9]uintptr{eglPlatformX11EXT, gfx.xDisplay, 0})
	}
	if d == 0 && gfx.fnEglGetDisplay != 0 {
		d = callHostC(gfx.fnEglGetDisplay, &[9]uintptr{gfx.xDisplay})
	}
	gfx.eglDisplay = d
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglGetDisplay(guest=%#x) -> X11 platform display %#x\n",
			nativeDisplay, d)
	}
	return d
}

//go:noinline
func goEglCreateWindowSurface(display, config, nativeWindow, attribs uintptr) uintptr {
	xid := nativeWindow
	gfx.mu.Lock()
	if w, ok := gfx.windows[nativeWindow]; ok {
		xid = w.xid
	}
	gfx.mu.Unlock()
	r := callHostC(gfx.fnEglCreateWindowSurface, &[9]uintptr{display, config, xid, attribs})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglCreateWindowSurface(anw=%#x) XID=%#x -> %#x\n",
			nativeWindow, xid, r)
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
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglMakeCurrent(dpy=%#x draw=%#x read=%#x ctx=%#x) -> %#x (count %d)\n",
			display, draw, read, ctx, r, atomic.LoadInt64(&eglMakeCurrentCount))
	}
	return r
}

//go:noinline
func goEglSwapBuffers(display, surface uintptr) uintptr {
	atomic.AddInt64(&eglSwapCount, 1)
	r := callHostC(gfx.fnEglSwapBuffers, &[9]uintptr{display, surface})
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: eglSwapBuffers(dpy=%#x surf=%#x) -> %#x (count %d)\n",
			display, surface, r, atomic.LoadInt64(&eglSwapCount))
	}
	return r
}

// BindGuestSurface publishes a guest-side stand-in for android.view.Surface as
// an ANativeWindow backed by this runtime's X11 window. The launch driver hands
// the engine a synthetic Surface handle in its lifecycle callbacks; if the
// engine passes that handle straight to eglCreateWindowSurface instead of going
// through ANativeWindow_fromSurface first, the wrapped create-window-surface
// must still find a real XID behind it. Handles the driver never registers keep
// their old behaviour (used as an XID verbatim).
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
	gfx.windows[surface] = &hostWindow{xid: gfx.xWindow, w: w, h: h, format: windowFormatRGBA8888}
	if debugGFX {
		fmt.Printf("rbxrt: gfx: guest surface %#x bound to XID %#x (%dx%d)\n",
			surface, gfx.xWindow, w, h)
	}
}

func goANativeWindowFromSurface(env, surface uintptr) uintptr {
	gfx.mu.Lock()
	defer gfx.mu.Unlock()
	p := gfx.nextWin
	gfx.nextWin += 16
	gfx.windows[p] = &hostWindow{
		xid:    gfx.xWindow,
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
