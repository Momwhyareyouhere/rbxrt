package hostabi

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

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
	fnXSelectInput        uintptr
	fnXPending            uintptr
	fnXNextEvent          uintptr

	display, window uintptr
	wmDelete        uintptr // WM_DELETE_WINDOW atom
}

// x11Event is the XEvent union (192 bytes). It is package-level so the address
// handed to XNextEvent is never on a movable goroutine stack.
var x11Event [24]uintptr

// Event masks and types from X.h.
const (
	x11KeyPressMask      = 1 << 0
	x11KeyReleaseMask    = 1 << 1
	x11ButtonPressMask   = 1 << 2
	x11ButtonReleaseMask = 1 << 3
	x11LeaveWindowMask   = 1 << 5
	x11PointerMotionMask = 1 << 6

	x11KeyPress      = 2
	x11KeyRelease    = 3
	x11ButtonPress   = 4
	x11ButtonRelease = 5
	x11MotionNotify  = 6
	x11LeaveNotify   = 8
	x11ClientMessage = 33
)

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
	x11.fnXSelectInput = hostDlsym(xlib, "XSelectInput")
	x11.fnXPending = hostDlsym(xlib, "XPending")
	x11.fnXNextEvent = hostDlsym(xlib, "XNextEvent")
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
	x11.display, x11.window = dpy, win
	x11SetupInput(xlib)
	callHostC(x11.fnXMapWindow, &[9]uintptr{dpy, win})
	callHostC(x11.fnXFlush, &[9]uintptr{dpy})

	gfx.platform = "x11"
	gfx.eglPlatform = eglPlatformX11EXT
	gfx.nativeDisplay = dpy
	gfx.nativeWindow = win
	return nil
}

// x11SetupInput asks for the window's mouse and keyboard events and for the
// window manager's close request (the title-bar close button).
func x11SetupInput(xlib uintptr) {
	if x11.fnXSelectInput == 0 || x11.fnXPending == 0 || x11.fnXNextEvent == 0 {
		fmt.Fprintf(os.Stderr, "rbxrt: graphics: libX11 lacks event functions; no mouse or keyboard input\n")
		return
	}
	dpy, win := x11.display, x11.window
	callHostC(x11.fnXSelectInput, &[9]uintptr{dpy, win, x11KeyPressMask | x11KeyReleaseMask |
		x11ButtonPressMask | x11ButtonReleaseMask | x11LeaveWindowMask | x11PointerMotionMask})
	// The user's keyboard layout, from the X server (xkb.go).
	if !xkbInitX11(dpy) {
		fmt.Fprintf(os.Stderr, "rbxrt: input: could not read the keyboard layout; using US\n")
	}
	// Held keys auto-repeat as press events only, not release/press pairs.
	if f := hostDlsym(xlib, "XkbSetDetectableAutoRepeat"); f != 0 {
		callHostC(f, &[9]uintptr{dpy, 1, 0})
	}
	intern := hostDlsym(xlib, "XInternAtom")
	setProtocols := hostDlsym(xlib, "XSetWMProtocols")
	if intern != 0 && setProtocols != 0 {
		x11.wmDelete = callHostC(intern, &[9]uintptr{dpy, CString("WM_DELETE_WINDOW"), 0})
		atoms := []uintptr{x11.wmDelete}
		x11Atoms = atoms
		callHostC(setProtocols, &[9]uintptr{dpy, win, uintptr(unsafe.Pointer(&atoms[0])), 1})
	}
}

var x11Atoms []uintptr // kept alive: Xlib copies it, but only during the call

// x11Pump drains the window's queued X events without blocking.
func x11Pump() {
	if x11.fnXPending == 0 || x11.display == 0 {
		return
	}
	ev := uintptr(unsafe.Pointer(&x11Event[0]))
	for callHostC(x11.fnXPending, &[9]uintptr{x11.display}) != 0 {
		callHostC(x11.fnXNextEvent, &[9]uintptr{x11.display, ev})
		x11Dispatch(ev)
	}
}

// x11Dispatch turns one XEvent into input. XKeyEvent, XButtonEvent and
// XMotionEvent share a layout: x, y at 64/68, state at 80, keycode/button at 84.
func x11Dispatch(ev uintptr) {
	i32 := func(off uintptr) int32 { return *(*int32)(unsafe.Pointer(ev + off)) }
	typ := i32(0)
	x, y := float32(i32(64)), float32(i32(68))
	state := uint32(i32(80))
	detail := uint32(i32(84))
	switch typ {
	case x11MotionNotify:
		pointerEvent(PointerMove, 0, x, y)
	case x11ButtonPress, x11ButtonRelease:
		switch detail {
		case 4, 5: // wheel up/down: one press per notch
			if typ == x11ButtonPress {
				s := float32(-1)
				if detail == 5 {
					s = 1
				}
				pushInput(InputEvent{Kind: InputPointer, Action: PointerScroll, X: x, Y: y, Scroll: s})
			}
			return
		}
		btn := map[uint32]int{1: ButtonLeft, 2: ButtonMiddle, 3: ButtonRight}[detail]
		if btn == 0 {
			return
		}
		action := PointerDown
		if typ == x11ButtonRelease {
			action = PointerUp
		}
		pointerEvent(action, btn, x, y)
	case x11LeaveNotify:
		pointerEvent(PointerLeave, 0, x, y)
	case x11KeyPress, x11KeyRelease:
		setCapsLock(state&2 != 0)                   // LockMask, as of before this event
		keyEvent(int(detail)-8, typ == x11KeyPress) // X keycodes are evdev + 8
	case x11ClientMessage:
		if x11.wmDelete != 0 && *(*uintptr)(unsafe.Pointer(ev + 56)) == x11.wmDelete {
			syscall.Kill(os.Getpid(), syscall.SIGINT)
		}
	}
}
