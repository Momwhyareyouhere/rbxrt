package hostabi

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// The native Wayland window backend: a wl_surface given the xdg_toplevel role,
// wrapped in a wl_egl_window, rendered into through Mesa's Wayland EGL
// platform. No Xwayland in between.
//
// libwayland-client is reached the same way as every other host library
// (dlopen + the guest-call trampoline). Its request helpers (wl_registry_bind,
// wl_compositor_create_surface, ...) are static inlines in the C headers, so
// they are spelled out here as the wl_proxy_marshal_flags calls they expand
// to. The core interfaces (wl_registry, wl_compositor, wl_surface) are data
// symbols exported by the library; xdg-shell is not part of libwayland, so its
// three interface descriptions are built below from the stable protocol XML.
// Event listeners are Go functions exposed to C through host thunks.
//
// Events are dispatched without a thread of our own: once at start-up until
// the compositor has configured the window, then non-blockingly before every
// eglSwapBuffers (Mesa dispatches its own private queue, not ours). That is
// enough to answer xdg_wm_base pings and acknowledge configures while the
// engine is rendering.

const (
	eglPlatformWaylandKHR = 0x31D8

	wlDisplayGetRegistry = 1
	wlRegistryBind       = 0
	wlCompositorCreate   = 0
	wlSurfaceCommit      = 6
	wlSurfaceAttach      = 1
	wlSurfaceDamage      = 2
	wlSeatGetPointer     = 0
	wlSeatGetKeyboard    = 1
	wlPointerSetCursor   = 0
	wlSeatCapPointer     = 1
	wlSeatCapKeyboard    = 2

	xdgWmBaseGetXdgSurface  = 2
	xdgWmBasePong           = 3
	xdgSurfaceGetToplevel   = 1
	xdgSurfaceAckConfigure  = 4
	xdgToplevelSetTitle     = 2
	xdgToplevelSetAppID     = 3
	wlMarshalFlagNone       = 0
	waylandCompositorWant   = 4 // wl_compositor version we bind (at most)
	waylandXdgWmBaseVersion = 1 // xdg-shell version we describe and bind

	// xdg-decoration (unstable v1): ask the compositor to draw the title bar
	// and window buttons. Without it a Wayland window has no frame at all.
	zxdgDecorationManagerGetToplevelDecoration = 1
	zxdgToplevelDecorationSetMode              = 1
	zxdgToplevelDecorationModeServerSide       = 2
)

// wlMessage and wlInterface mirror struct wl_message and struct wl_interface
// from wayland-util.h (x86_64 layout).
type wlMessage struct {
	name, signature, types uintptr
}

type wlInterface struct {
	name        uintptr
	version     int32
	methodCount int32
	methods     uintptr
	eventCount  int32
	_           int32
	events      uintptr
}

type waylandState struct {
	client, eglLib uintptr

	fnConnect, fnRoundtrip, fnDispatchPending, fnFlush, fnGetFD uintptr
	fnPrepareRead, fnReadEvents, fnCancelRead                   uintptr
	fnMarshalFlags, fnAddListener, fnGetVersion                 uintptr
	fnEglWindowCreate                                           uintptr

	registryIface, compositorIface, surfaceIface     uintptr
	seatIface, pointerIface, keyboardIface, shmIface uintptr

	// Input: the first wl_seat, its pointer and keyboard (all version 1).
	seatName, shmName       uintptr
	seat, pointer, keyboard uintptr
	shm                     uintptr
	cursorSurface           uintptr
	cursorHotX, cursorHotY  uintptr

	display, registry, compositor, wmBase uintptr
	surface, xdgSurface, toplevel, eglWin uintptr
	compositorName, compositorVer         uintptr
	wmBaseName                            uintptr
	decorationMgrName                     uintptr
	decorationMgr, decoration             uintptr
	configured                            bool
	fd                                    int
}

var wl waylandState

// The xdg-shell interface descriptions and listener tables. They are handed to
// C by address, so they live in package-level variables that never move or die.
var (
	xdgWmBaseIface, xdgSurfaceIface, xdgToplevelIface wlInterface

	xdgWmBaseRequests, xdgSurfaceRequests, xdgToplevelRequests []wlMessage
	xdgWmBaseEvents, xdgSurfaceEvents, xdgToplevelEvents       []wlMessage

	wlNullTypes        [8]uintptr
	xdgGetSurfaceTypes [2]uintptr
	xdgGetToplevelType [1]uintptr

	zxdgDecorationMgrIface, zxdgToplevelDecorationIface       wlInterface
	zxdgDecorationMgrRequests, zxdgToplevelDecorationRequests []wlMessage
	zxdgToplevelDecorationEvents                              []wlMessage
	zxdgGetDecorationTypes                                    [2]uintptr

	wlRegistryListener  [2]uintptr
	wlSeatListener      [1]uintptr
	wlPointerListener   [5]uintptr
	wlKeyboardListener  [5]uintptr
	xdgWmBaseListener   [1]uintptr
	xdgSurfaceListener  [1]uintptr
	xdgToplevelListener [2]uintptr
)

func wlMsg(name, sig string, types uintptr) wlMessage {
	if types == 0 {
		types = uintptr(unsafe.Pointer(&wlNullTypes[0]))
	}
	return wlMessage{CString(name), CString(sig), types}
}

func wlIface(i *wlInterface, name string, version int32, req, ev []wlMessage) {
	*i = wlInterface{name: CString(name), version: version,
		methodCount: int32(len(req)), eventCount: int32(len(ev))}
	if len(req) > 0 {
		i.methods = uintptr(unsafe.Pointer(&req[0]))
	}
	if len(ev) > 0 {
		i.events = uintptr(unsafe.Pointer(&ev[0]))
	}
}

// buildXdgShell fills in xdg_wm_base, xdg_surface and xdg_toplevel as of
// version 1 of the stable xdg-shell protocol. Only version-1 events are
// described, and we bind version 1, so the compositor sends nothing else.
func buildXdgShell() {
	xdgGetSurfaceTypes = [2]uintptr{uintptr(unsafe.Pointer(&xdgSurfaceIface)), wl.surfaceIface}
	xdgGetToplevelType = [1]uintptr{uintptr(unsafe.Pointer(&xdgToplevelIface))}

	xdgWmBaseRequests = []wlMessage{
		wlMsg("destroy", "", 0),
		wlMsg("create_positioner", "n", 0),
		wlMsg("get_xdg_surface", "no", uintptr(unsafe.Pointer(&xdgGetSurfaceTypes[0]))),
		wlMsg("pong", "u", 0),
	}
	xdgWmBaseEvents = []wlMessage{wlMsg("ping", "u", 0)}

	xdgSurfaceRequests = []wlMessage{
		wlMsg("destroy", "", 0),
		wlMsg("get_toplevel", "n", uintptr(unsafe.Pointer(&xdgGetToplevelType[0]))),
		wlMsg("get_popup", "n?oo", 0),
		wlMsg("set_window_geometry", "iiii", 0),
		wlMsg("ack_configure", "u", 0),
	}
	xdgSurfaceEvents = []wlMessage{wlMsg("configure", "u", 0)}

	xdgToplevelRequests = []wlMessage{
		wlMsg("destroy", "", 0),
		wlMsg("set_parent", "?o", 0),
		wlMsg("set_title", "s", 0),
		wlMsg("set_app_id", "s", 0),
		wlMsg("show_window_menu", "ouii", 0),
		wlMsg("move", "ou", 0),
		wlMsg("resize", "ouu", 0),
		wlMsg("set_max_size", "ii", 0),
		wlMsg("set_min_size", "ii", 0),
		wlMsg("set_maximized", "", 0),
		wlMsg("unset_maximized", "", 0),
		wlMsg("set_fullscreen", "?o", 0),
		wlMsg("unset_fullscreen", "", 0),
		wlMsg("set_minimized", "", 0),
	}
	xdgToplevelEvents = []wlMessage{
		wlMsg("configure", "iia", 0),
		wlMsg("close", "", 0),
	}

	wlIface(&xdgWmBaseIface, "xdg_wm_base", waylandXdgWmBaseVersion, xdgWmBaseRequests, xdgWmBaseEvents)
	wlIface(&xdgSurfaceIface, "xdg_surface", waylandXdgWmBaseVersion, xdgSurfaceRequests, xdgSurfaceEvents)
	wlIface(&xdgToplevelIface, "xdg_toplevel", waylandXdgWmBaseVersion, xdgToplevelRequests, xdgToplevelEvents)

	zxdgGetDecorationTypes = [2]uintptr{
		uintptr(unsafe.Pointer(&zxdgToplevelDecorationIface)), uintptr(unsafe.Pointer(&xdgToplevelIface))}
	zxdgDecorationMgrRequests = []wlMessage{
		wlMsg("destroy", "", 0),
		wlMsg("get_toplevel_decoration", "no", uintptr(unsafe.Pointer(&zxdgGetDecorationTypes[0]))),
	}
	zxdgToplevelDecorationRequests = []wlMessage{
		wlMsg("destroy", "", 0),
		wlMsg("set_mode", "u", 0),
		wlMsg("unset_mode", "", 0),
	}
	zxdgToplevelDecorationEvents = []wlMessage{wlMsg("configure", "u", 0)}
	wlIface(&zxdgDecorationMgrIface, "zxdg_decoration_manager_v1", 1, zxdgDecorationMgrRequests, nil)
	wlIface(&zxdgToplevelDecorationIface, "zxdg_toplevel_decoration_v1", 1,
		zxdgToplevelDecorationRequests, zxdgToplevelDecorationEvents)

	wlRegistryListener = [2]uintptr{mkThunk(funcAddr(wlOnGlobal)), mkThunk(funcAddr(wlOnGlobalRemove))}
	xdgWmBaseListener = [1]uintptr{mkThunk(funcAddr(xdgOnPing))}
	xdgSurfaceListener = [1]uintptr{mkThunk(funcAddr(xdgOnSurfaceConfigure))}
	xdgToplevelListener = [2]uintptr{mkThunk(funcAddr(xdgOnToplevelConfigure)), mkThunk(funcAddr(xdgOnClose))}
	wlSeatListener = [1]uintptr{mkThunk(funcAddr(wlOnSeatCapabilities))}
	wlPointerListener = [5]uintptr{
		mkThunk(funcAddr(wlOnPointerEnter)), mkThunk(funcAddr(wlOnPointerLeave)),
		mkThunk(funcAddr(wlOnPointerMotion)), mkThunk(funcAddr(wlOnPointerButton)),
		mkThunk(funcAddr(wlOnPointerAxis))}
	wlKeyboardListener = [5]uintptr{
		mkThunk(funcAddr(wlOnKeymap)), mkThunk(funcAddr(wlOnKeyboardEnter)),
		mkThunk(funcAddr(wlOnKeyboardLeave)), mkThunk(funcAddr(wlOnKey)),
		mkThunk(funcAddr(wlOnModifiers))}
	arenaMakeExec()
}

// wlMarshal sends request opcode on proxy (wl_proxy_marshal_flags). iface is
// the interface of the object a new_id argument creates (0 if none); the
// result is that new proxy. At most four request arguments fit.
//
// wl_proxy_marshal_flags is variadic, so AL should hold the number of vector
// registers used. The trampoline leaves the ninth slot (the fourth request
// argument, 0 when absent) in AX; a nonzero AL only makes the callee spill the
// vector registers, which is harmless.
func wlMarshal(proxy, opcode, iface, version uintptr, args ...uintptr) uintptr {
	var a [9]uintptr
	a[0], a[1], a[2], a[3], a[4] = proxy, opcode, iface, version, wlMarshalFlagNone
	copy(a[5:9], args)
	return callHostC(wl.fnMarshalFlags, &a)
}

func wlVersion(proxy uintptr) uintptr {
	return callHostC(wl.fnGetVersion, &[9]uintptr{proxy})
}

func wlListen(proxy uintptr, listener *uintptr) {
	callHostC(wl.fnAddListener, &[9]uintptr{proxy, uintptr(unsafe.Pointer(listener)), 0})
}

// openWayland connects to $WAYLAND_DISPLAY and maps one w x h toplevel.
func openWayland(w, h int) error {
	if wl.client = hostDlopen("libwayland-client.so.0", rtldNow|rtldGlobal); wl.client == 0 {
		return fmt.Errorf("cannot dlopen libwayland-client.so.0")
	}
	if wl.eglLib = hostDlopen("libwayland-egl.so.1", rtldNow|rtldGlobal); wl.eglLib == 0 {
		return fmt.Errorf("cannot dlopen libwayland-egl.so.1")
	}
	syms := map[string]*uintptr{
		"wl_display_connect":          &wl.fnConnect,
		"wl_display_roundtrip":        &wl.fnRoundtrip,
		"wl_display_dispatch_pending": &wl.fnDispatchPending,
		"wl_display_flush":            &wl.fnFlush,
		"wl_display_get_fd":           &wl.fnGetFD,
		"wl_display_prepare_read":     &wl.fnPrepareRead,
		"wl_display_read_events":      &wl.fnReadEvents,
		"wl_display_cancel_read":      &wl.fnCancelRead,
		"wl_proxy_marshal_flags":      &wl.fnMarshalFlags,
		"wl_proxy_add_listener":       &wl.fnAddListener,
		"wl_proxy_get_version":        &wl.fnGetVersion,
		"wl_registry_interface":       &wl.registryIface,
		"wl_compositor_interface":     &wl.compositorIface,
		"wl_surface_interface":        &wl.surfaceIface,
		"wl_seat_interface":           &wl.seatIface,
		"wl_pointer_interface":        &wl.pointerIface,
		"wl_keyboard_interface":       &wl.keyboardIface,
		"wl_shm_interface":            &wl.shmIface,
	}
	for name, p := range syms {
		if *p = hostDlsym(wl.client, name); *p == 0 {
			return fmt.Errorf("libwayland-client is missing %s (needs libwayland >= 1.20)", name)
		}
	}
	if wl.fnEglWindowCreate = hostDlsym(wl.eglLib, "wl_egl_window_create"); wl.fnEglWindowCreate == 0 {
		return fmt.Errorf("libwayland-egl is missing wl_egl_window_create")
	}
	buildXdgShell()

	if wl.display = callHostC(wl.fnConnect, &[9]uintptr{0}); wl.display == 0 {
		return fmt.Errorf("cannot connect to Wayland display %q", os.Getenv("WAYLAND_DISPLAY"))
	}
	wl.fd = int(int32(callHostC(wl.fnGetFD, &[9]uintptr{wl.display})))

	wl.registry = wlMarshal(wl.display, wlDisplayGetRegistry, wl.registryIface, wlVersion(wl.display), 0)
	if wl.registry == 0 {
		return fmt.Errorf("wl_display.get_registry failed")
	}
	wlListen(wl.registry, &wlRegistryListener[0])
	if int32(callHostC(wl.fnRoundtrip, &[9]uintptr{wl.display})) < 0 {
		return fmt.Errorf("Wayland roundtrip failed")
	}
	if wl.compositorName == 0 {
		return fmt.Errorf("compositor does not advertise wl_compositor")
	}
	if wl.wmBaseName == 0 {
		return fmt.Errorf("compositor does not advertise xdg_wm_base (xdg-shell)")
	}

	ver := wl.compositorVer
	if ver > waylandCompositorWant {
		ver = waylandCompositorWant
	}
	wl.compositor = wlMarshal(wl.registry, wlRegistryBind, wl.compositorIface, ver,
		wl.compositorName, CString("wl_compositor"), ver, 0)
	wl.wmBase = wlMarshal(wl.registry, wlRegistryBind, uintptr(unsafe.Pointer(&xdgWmBaseIface)),
		waylandXdgWmBaseVersion, wl.wmBaseName, CString("xdg_wm_base"), waylandXdgWmBaseVersion, 0)
	if wl.compositor == 0 || wl.wmBase == 0 {
		return fmt.Errorf("binding wl_compositor/xdg_wm_base failed")
	}
	wlListen(wl.wmBase, &xdgWmBaseListener[0])

	wl.surface = wlMarshal(wl.compositor, wlCompositorCreate, wl.surfaceIface, wlVersion(wl.compositor), 0)
	if wl.surface == 0 {
		return fmt.Errorf("wl_compositor.create_surface failed")
	}
	wl.xdgSurface = wlMarshal(wl.wmBase, xdgWmBaseGetXdgSurface, uintptr(unsafe.Pointer(&xdgSurfaceIface)),
		waylandXdgWmBaseVersion, 0, wl.surface)
	wlListen(wl.xdgSurface, &xdgSurfaceListener[0])
	wl.toplevel = wlMarshal(wl.xdgSurface, xdgSurfaceGetToplevel, uintptr(unsafe.Pointer(&xdgToplevelIface)),
		waylandXdgWmBaseVersion, 0)
	if wl.xdgSurface == 0 || wl.toplevel == 0 {
		return fmt.Errorf("creating the xdg_toplevel failed")
	}
	wlListen(wl.toplevel, &xdgToplevelListener[0])
	wlMarshal(wl.toplevel, xdgToplevelSetTitle, 0, waylandXdgWmBaseVersion, CString("rbxrt"))
	wlMarshal(wl.toplevel, xdgToplevelSetAppID, 0, waylandXdgWmBaseVersion, CString("rbxrt"))

	// Server-side decorations (title bar, close/minimize/maximize buttons) on
	// compositors that offer them: KDE, sway, Hyprland, wlroots-based ones.
	// GNOME does not, and the window stays frameless there.
	if wl.decorationMgrName != 0 {
		wl.decorationMgr = wlMarshal(wl.registry, wlRegistryBind, uintptr(unsafe.Pointer(&zxdgDecorationMgrIface)),
			1, wl.decorationMgrName, CString("zxdg_decoration_manager_v1"), 1, 0)
		if wl.decorationMgr != 0 {
			wl.decoration = wlMarshal(wl.decorationMgr, zxdgDecorationManagerGetToplevelDecoration,
				uintptr(unsafe.Pointer(&zxdgToplevelDecorationIface)), 1, 0, wl.toplevel)
			if wl.decoration != 0 {
				wlMarshal(wl.decoration, zxdgToplevelDecorationSetMode, 0, 1, zxdgToplevelDecorationModeServerSide)
			}
		}
	} else if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: compositor has no xdg-decoration; window has no title bar\n")
	}

	// Input. Version 1 of wl_seat (and so of its pointer and keyboard) has
	// every event we need and nothing we would have to answer.
	if wl.seatName != 0 {
		wl.seat = wlMarshal(wl.registry, wlRegistryBind, wl.seatIface, 1,
			wl.seatName, CString("wl_seat"), 1, 0)
		if wl.seat != 0 {
			wlListen(wl.seat, &wlSeatListener[0])
		}
	} else {
		fmt.Fprintf(os.Stderr, "rbxrt: graphics: compositor has no wl_seat; no mouse or keyboard input\n")
	}
	waylandLoadCursor()

	// The first commit carries no buffer; the compositor answers it with the
	// initial configure, which must be acknowledged before a buffer is attached.
	wlMarshal(wl.surface, wlSurfaceCommit, 0, wlVersion(wl.surface))
	for i := 0; !wl.configured; i++ {
		if i == 100 || int32(callHostC(wl.fnRoundtrip, &[9]uintptr{wl.display})) < 0 {
			return fmt.Errorf("compositor never configured the window")
		}
	}

	if wl.eglWin = callHostC(wl.fnEglWindowCreate, &[9]uintptr{wl.surface, uintptr(w), uintptr(h)}); wl.eglWin == 0 {
		return fmt.Errorf("wl_egl_window_create failed")
	}

	gfx.platform = "wayland"
	gfx.eglPlatform = eglPlatformWaylandKHR
	gfx.nativeDisplay = wl.display
	gfx.nativeWindow = wl.eglWin
	return nil
}

// waylandPump dispatches whatever events the compositor has sent, without
// blocking. It runs before every eglSwapBuffers.
func waylandPump() {
	d := &[9]uintptr{wl.display}
	for int32(callHostC(wl.fnPrepareRead, d)) != 0 {
		callHostC(wl.fnDispatchPending, d)
	}
	callHostC(wl.fnFlush, d)
	pfd := struct {
		fd             int32
		events, revent int16
	}{fd: int32(wl.fd), events: 1} // POLLIN
	n, _, _ := syscall.RawSyscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&pfd)), 1, 0)
	if int(n) > 0 {
		callHostC(wl.fnReadEvents, d)
	} else {
		callHostC(wl.fnCancelRead, d)
	}
	callHostC(wl.fnDispatchPending, d)
}

// --- listeners (called from libwayland through host thunks) ---------------

//go:noinline
func wlOnGlobal(data, registry, name, iface, version uintptr) {
	switch cstr(iface) {
	case "wl_compositor":
		wl.compositorName, wl.compositorVer = uintptr(uint32(name)), uintptr(uint32(version))
	case "xdg_wm_base":
		wl.wmBaseName = uintptr(uint32(name))
	case "wl_seat":
		if wl.seatName == 0 {
			wl.seatName = uintptr(uint32(name))
		}
	case "wl_shm":
		wl.shmName = uintptr(uint32(name))
	case "zxdg_decoration_manager_v1":
		wl.decorationMgrName = uintptr(uint32(name))
	}
}

//go:noinline
func wlOnGlobalRemove(data, registry, name uintptr) {}

//go:noinline
func xdgOnPing(data, wmBase, serial uintptr) {
	wlMarshal(wmBase, xdgWmBasePong, 0, waylandXdgWmBaseVersion, uintptr(uint32(serial)))
}

//go:noinline
func xdgOnSurfaceConfigure(data, xdgSurface, serial uintptr) {
	wlMarshal(xdgSurface, xdgSurfaceAckConfigure, 0, waylandXdgWmBaseVersion, uintptr(uint32(serial)))
	// Later acks take effect with the next commit, which Mesa makes on the
	// next swap.
	wl.configured = true
}

// The engine renders at the size it was given at start-up, so a size suggested
// by the compositor is not applied: the buffer keeps its size and the
// compositor places it (xdg-shell allows a buffer that differs from the
// suggestion).
//
//go:noinline
func xdgOnToplevelConfigure(data, toplevel, width, height, states uintptr) {
	if debugGFX {
		fmt.Fprintf(os.Stderr, "rbxrt: gfx: xdg_toplevel.configure %dx%d\n", int32(width), int32(height))
	}
}

// Closing the window stops the runtime the same way Ctrl-C does.
//
//go:noinline
func xdgOnClose(data, toplevel uintptr) {
	syscall.Kill(os.Getpid(), syscall.SIGINT)
}

// --- input ----------------------------------------------------------------

// waylandLoadCursor prepares the arrow cursor shown over the window. A
// Wayland client must set its own cursor image; without one the pointer is
// invisible over the window. It uses the system cursor theme through
// libwayland-cursor ($XCURSOR_THEME, $XCURSOR_SIZE) and is skipped quietly if
// that is unavailable.
func waylandLoadCursor() {
	if wl.shmName == 0 {
		return
	}
	lib := hostDlopen("libwayland-cursor.so.0", rtldNow|rtldGlobal)
	if lib == 0 {
		return
	}
	themeLoad := hostDlsym(lib, "wl_cursor_theme_load")
	getCursor := hostDlsym(lib, "wl_cursor_theme_get_cursor")
	getBuffer := hostDlsym(lib, "wl_cursor_image_get_buffer")
	if themeLoad == 0 || getCursor == 0 || getBuffer == 0 {
		return
	}
	wl.shm = wlMarshal(wl.registry, wlRegistryBind, wl.shmIface, 1, wl.shmName, CString("wl_shm"), 1, 0)
	if wl.shm == 0 {
		return
	}
	var themeName uintptr
	if t := os.Getenv("XCURSOR_THEME"); t != "" {
		themeName = CString(t)
	}
	theme := callHostC(themeLoad, &[9]uintptr{themeName, uintptr(envInt("XCURSOR_SIZE", 24)), wl.shm})
	if theme == 0 {
		return
	}
	var cursor uintptr
	for _, name := range []string{"default", "left_ptr"} {
		if cursor = callHostC(getCursor, &[9]uintptr{theme, CString(name)}); cursor != 0 {
			break
		}
	}
	// struct wl_cursor { unsigned image_count; struct wl_cursor_image **images; char *name; }
	if cursor == 0 || *(*uint32)(unsafe.Pointer(cursor)) == 0 {
		return
	}
	image := *(*uintptr)(unsafe.Pointer(*(*uintptr)(unsafe.Pointer(cursor + 8))))
	// struct wl_cursor_image { uint32 width, height, hotspot_x, hotspot_y, delay; }
	iw := *(*uint32)(unsafe.Pointer(image))
	ih := *(*uint32)(unsafe.Pointer(image + 4))
	wl.cursorHotX = uintptr(*(*uint32)(unsafe.Pointer(image + 8)))
	wl.cursorHotY = uintptr(*(*uint32)(unsafe.Pointer(image + 12)))
	buf := callHostC(getBuffer, &[9]uintptr{image})
	if buf == 0 {
		return
	}
	surf := wlMarshal(wl.compositor, wlCompositorCreate, wl.surfaceIface, wlVersion(wl.compositor), 0)
	if surf == 0 {
		return
	}
	wlMarshal(surf, wlSurfaceAttach, 0, wlVersion(surf), buf, 0, 0)
	wlMarshal(surf, wlSurfaceDamage, 0, wlVersion(surf), 0, 0, uintptr(iw), uintptr(ih))
	wlMarshal(surf, wlSurfaceCommit, 0, wlVersion(surf))
	wl.cursorSurface = surf
}

func wlFixed(v uintptr) float32 { return float32(int32(uint32(v))) / 256 }

//go:noinline
func wlOnSeatCapabilities(data, seat, caps uintptr) {
	if caps&wlSeatCapPointer != 0 && wl.pointer == 0 {
		if wl.pointer = wlMarshal(seat, wlSeatGetPointer, wl.pointerIface, 1, 0); wl.pointer != 0 {
			wlListen(wl.pointer, &wlPointerListener[0])
		}
	}
	if caps&wlSeatCapKeyboard != 0 && wl.keyboard == 0 {
		if wl.keyboard = wlMarshal(seat, wlSeatGetKeyboard, wl.keyboardIface, 1, 0); wl.keyboard != 0 {
			wlListen(wl.keyboard, &wlKeyboardListener[0])
		}
	}
}

//go:noinline
func wlOnPointerEnter(data, pointer, serial, surface, x, y uintptr) {
	if wl.cursorSurface != 0 {
		wlMarshal(pointer, wlPointerSetCursor, 0, 1, uintptr(uint32(serial)), wl.cursorSurface,
			wl.cursorHotX, wl.cursorHotY)
	}
	pointerEvent(PointerMove, 0, wlFixed(x), wlFixed(y))
}

//go:noinline
func wlOnPointerLeave(data, pointer, serial, surface uintptr) {
	x, y := pointerAt()
	pointerEvent(PointerLeave, 0, x, y)
}

//go:noinline
func wlOnPointerMotion(data, pointer, time, x, y uintptr) {
	pointerEvent(PointerMove, 0, wlFixed(x), wlFixed(y))
}

//go:noinline
func wlOnPointerButton(data, pointer, serial, time, button, state uintptr) {
	x, y := pointerAt()
	action := PointerUp
	if uint32(state) == 1 { // WL_POINTER_BUTTON_STATE_PRESSED
		action = PointerDown
	}
	pointerEvent(action, int(uint32(button)), x, y)
}

//go:noinline
func wlOnPointerAxis(data, pointer, time, axis, value uintptr) {
	if uint32(axis) != 0 { // only WL_POINTER_AXIS_VERTICAL_SCROLL
		return
	}
	x, y := pointerAt()
	pushInput(InputEvent{Kind: InputPointer, Action: PointerScroll, X: x, Y: y,
		Scroll: wlFixed(value) / 10}) // one wheel notch is 10 units
}

// The compositor's keymap (the user's keyboard layout) drives the characters
// keys type; see xkb.go. It arrives as a file to map; the fd must be closed.
//
//go:noinline
func wlOnKeymap(data, keyboard, format, fd, size uintptr) {
	f := int(int32(fd))
	defer syscall.Close(f)
	if uint32(format) != 1 || size == 0 { // WL_KEYBOARD_KEYMAP_FORMAT_XKB_V1
		return
	}
	b, err := syscall.Mmap(f, 0, int(uint32(size)), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return
	}
	text := b
	if i := len(text) - 1; i >= 0 && text[i] == 0 {
		text = text[:i]
	}
	if !xkbLoadKeymapText(text) {
		fmt.Fprintf(os.Stderr, "rbxrt: input: could not load the keyboard layout; using US\n")
	}
	syscall.Munmap(b)
}

//go:noinline
func wlOnKeyboardEnter(data, keyboard, serial, surface, keys uintptr) {}

// Keys still held when focus moves elsewhere would otherwise stay down in the
// engine: release the modifiers (the engine sees their key-up).
//
//go:noinline
func wlOnKeyboardLeave(data, keyboard, serial, surface uintptr) {
	stopRepeat(0)
	for _, k := range []int{29, 42, 54, 56, 97, 100, 125, 126} {
		input.mu.Lock()
		held := input.meta&metaForKey(evdevToAndroid(k)) != 0
		input.mu.Unlock()
		if held {
			keyEvent(k, false)
		}
	}
}

//go:noinline
func wlOnKey(data, keyboard, serial, time, key, state uintptr) {
	down := uint32(state) == 1 // WL_KEYBOARD_KEY_STATE_PRESSED
	keyEvent(int(uint32(key)), down)
	if down {
		startRepeat(int(uint32(key)))
	} else {
		stopRepeat(int(uint32(key)))
	}
}

//go:noinline
func wlOnModifiers(data, keyboard, serial, depressed, latched, locked, group uintptr) {
	setCapsLock(uint32(locked)&2 != 0) // xkb "Lock" modifier
	xkbUpdateMask(depressed, latched, locked, group)
}
