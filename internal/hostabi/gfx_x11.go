package hostabi

import "fmt"

// The X11 window backend: one Xlib window, rendered into through Mesa's X11
// EGL platform. Used on X11 sessions, and on Wayland sessions through Xwayland
// when RBXRT_DISPLAY=x11 or the native Wayland backend cannot start.

const eglPlatformX11EXT = 0x31D5

type x11State struct {
	lib uintptr

	fnXOpenDisplay        uintptr
	fnXDefaultRootWindow  uintptr
	fnXCreateSimpleWindow uintptr
	fnXMapWindow          uintptr
	fnXStoreName          uintptr
	fnXFlush              uintptr
}

var x11 x11State

// openX11 opens the X display named by $DISPLAY and maps one w x h window.
func openX11(w, h int) error {
	xlib := hostDlopen("libX11.so.6", rtldNow|rtldGlobal)
	if xlib == 0 {
		return fmt.Errorf("cannot dlopen libX11.so.6")
	}
	x11.lib = xlib
	x11.fnXOpenDisplay = hostDlsym(xlib, "XOpenDisplay")
	x11.fnXDefaultRootWindow = hostDlsym(xlib, "XDefaultRootWindow")
	x11.fnXCreateSimpleWindow = hostDlsym(xlib, "XCreateSimpleWindow")
	x11.fnXMapWindow = hostDlsym(xlib, "XMapWindow")
	x11.fnXStoreName = hostDlsym(xlib, "XStoreName")
	x11.fnXFlush = hostDlsym(xlib, "XFlush")
	if x11.fnXOpenDisplay == 0 || x11.fnXCreateSimpleWindow == 0 || x11.fnXMapWindow == 0 {
		return fmt.Errorf("libX11 is missing the window-creation entry points")
	}

	dpy := callHostC(x11.fnXOpenDisplay, &[9]uintptr{0})
	if dpy == 0 {
		return fmt.Errorf("XOpenDisplay failed")
	}
	root := callHostC(x11.fnXDefaultRootWindow, &[9]uintptr{dpy})
	win := callHostC(x11.fnXCreateSimpleWindow, &[9]uintptr{
		dpy, root, 60, 60, uintptr(w), uintptr(h), 0, 0, 0})
	if win == 0 {
		return fmt.Errorf("XCreateSimpleWindow failed")
	}
	callHostC(x11.fnXStoreName, &[9]uintptr{dpy, win, CString("rbxrt")})
	callHostC(x11.fnXMapWindow, &[9]uintptr{dpy, win})
	callHostC(x11.fnXFlush, &[9]uintptr{dpy})

	gfx.platform = "x11"
	gfx.eglPlatform = eglPlatformX11EXT
	gfx.nativeDisplay = dpy
	gfx.nativeWindow = win
	return nil
}
