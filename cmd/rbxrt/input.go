package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"rbxrt/internal/hostabi"
	"rbxrt/internal/loader"
)

// Input delivery: the runtime plays the part of Roblox's Java input layer.
// Mouse and keyboard events from the host window (hostabi.PollInput) go to the
// engine through the natives the app's own view calls (recovered from the
// APK's dex, class uk.f and h):
//
//	NativeInputInterface.nativePassMouseMove(x, y, dx, dy)      on HOVER_MOVE/MOVE
//	NativeInputInterface.nativePassMouseButton(x, y, down, btn) on BUTTON_PRESS/RELEASE,
//	                                                            btn = actionButton-1
//	NativeInputInterface.nativePassMouseWheel(x, y, vscroll)    on SCROLL
//	NativeGLInterface.nativePassKeyEvent(down, scanCode, keyCode, isRepeat)
//
// Positions are in dp (pixels / DeviceUtils.getScreenDensity()); the runtime
// reports a density of 1, so they are window pixels. Roblox handles a mouse
// on Android (Chromebooks) the way it does on desktop: click buttons, hold the
// right button to turn the camera, wheel to zoom. The calls are made on the
// "Java main" thread between message pumps, where the app makes them.
//
// Text boxes do not read key events. When one gains focus the engine asks Java
// to show the on-screen keyboard (NativeGLJavaInterface.showKeyboard with the
// text box's native handle) and hides it on focus loss; the app's keyboard
// (RbxKeyboard) then reports every edit with
// NativeGLInterface.syncTextboxTextAndCursorPosition2(text, cursor), and Enter
// with nativeReturnPressedFromOnScreenKeyboard(textBox) followed by
// nativePassText(textBox, text, true, cursor), which commits the text and
// ends editing (with false, as RbxKeyboard also sends per edit, the engine
// ignores the call). The runtime is that keyboard: while a text box is
// focused, typed characters, Backspace and Enter edit its text instead of
// going out as key events.

const (
	inputClass = "Java_com_roblox_engine_jni_NativeInputInterface_"
	glClass    = "Java_com_roblox_engine_jni_NativeGLInterface_"

	// MotionEvent.BUTTON_* minus one, as the app passes them.
	mouseButtonPrimary   = 0 // BUTTON_PRIMARY (1)
	mouseButtonSecondary = 1 // BUTTON_SECONDARY (2)
	mouseButtonTertiary  = 3 // BUTTON_TERTIARY (4)

	screenDensity = 1 // DisplayMetrics.density reported to the app (javadata.go)
)

var debugInput = os.Getenv("RBXRT_DEBUG_INPUT") != ""

// textInput tracks the focused text box (0 when none) and what has been typed
// into each box. The engine does not hand over a box's current text (it comes
// as a byte[], and the JNI layer has no arrays), so a box starts empty the
// first time it is focused and keeps what was typed when refocused.
var textInput struct {
	mu     sync.Mutex
	active uintptr
	text   map[uintptr][]rune
}

func setupTextInput() {
	textInput.text = map[uintptr][]rune{}
	hostabi.SetFakeMethodArgFunc(classGLJava, "showKeyboard", func(box uintptr) uintptr {
		textInput.mu.Lock()
		textInput.active = box
		textInput.mu.Unlock()
		if debugInput {
			fmt.Printf("input: text box %#x focused\n", box)
		}
		return 0
	})
	hostabi.SetFakeMethodFunc(classGLJava, "hideKeyboard", func() uintptr {
		textInput.mu.Lock()
		textInput.active = 0
		textInput.mu.Unlock()
		if debugInput {
			fmt.Printf("input: text box unfocused\n")
		}
		return 0
	})
}

// editText applies a key press to the focused text box. It reports whether
// the key was consumed (and so must not also go out as a key event); the new
// text, if it changed; and whether Enter was pressed.
func editText(e hostabi.InputEvent) (box uintptr, text []rune, changed, enter, consumed bool) {
	textInput.mu.Lock()
	defer textInput.mu.Unlock()
	box = textInput.active
	if box == 0 {
		return 0, nil, false, false, false
	}
	const (
		keycodeDel     = 67
		keycodeEnter   = 66
		keycodeNumpadE = 160
		modifiers      = 0x1000 | 0x2 | 0x10000 // META_CTRL_ON | META_ALT_ON | META_META_ON
	)
	t := textInput.text[box]
	switch {
	case e.KeyCode == keycodeEnter || e.KeyCode == keycodeNumpadE:
		return box, t, false, e.Down, true
	case e.KeyCode == keycodeDel:
		if e.Down && len(t) > 0 {
			t = t[:len(t)-1]
			textInput.text[box] = t
			return box, t, true, false, true
		}
		return box, t, false, false, true
	case e.Unicode >= ' ' && e.Meta&modifiers == 0:
		if e.Down {
			t = append(t, e.Unicode)
			textInput.text[box] = t
			return box, t, true, false, true
		}
		return box, t, false, false, true
	}
	return box, t, false, false, false
}

// inputDriver returns the per-iteration input step for the main loop, or nil
// when the engine does not export the input natives.
func inputDriver(im *loader.Image) func() {
	exp := func(name string) uintptr {
		fn, ok := im.Exports[name]
		if !ok {
			fmt.Printf("input: %s is not exported\n", name)
		}
		return fn
	}
	moveFn := exp(inputClass + "nativePassMouseMove")
	buttonFn := exp(inputClass + "nativePassMouseButton")
	wheelFn := exp(inputClass + "nativePassMouseWheel")
	keyFn := exp(glClass + "nativePassKeyEvent")
	textFn := exp(glClass + "nativePassText")
	syncFn := exp(glClass + "syncTextboxTextAndCursorPosition2")
	returnFn := exp(glClass + "nativeReturnPressedFromOnScreenKeyboard")
	if moveFn == 0 || buttonFn == 0 || wheelFn == 0 || keyFn == 0 {
		fmt.Printf("input: mouse and keyboard disabled\n")
		return nil
	}
	env := hostabi.JNIEnvAddr()
	inputCls := hostabi.FindClassHandle("com/roblox/engine/jni/NativeInputInterface")
	glCls := hostabi.FindClassHandle("com/roblox/engine/jni/NativeGLInterface")

	var lastX, lastY float32
	held := map[int]bool{} // keys down, to flag auto-repeat
	bool2 := func(b bool) uintptr {
		if b {
			return 1
		}
		return 0
	}

	if textFn != 0 && syncFn != 0 && returnFn != 0 {
		setupTextInput()
	}
	go replayInput(os.Getenv("RBXRT_INPUT_REPLAY"))

	return func() {
		for _, e := range hostabi.PollInput() {
			switch e.Kind {
			case hostabi.InputPointer:
				x, y := e.X/screenDensity, e.Y/screenDensity
				switch e.Action {
				case hostabi.PointerMove:
					hostabi.CallGuestWide(moveFn, []uintptr{env, inputCls}, x, y, x-lastX, y-lastY)
					lastX, lastY = x, y
				case hostabi.PointerDown, hostabi.PointerUp:
					btn, ok := map[int]uintptr{hostabi.ButtonLeft: mouseButtonPrimary,
						hostabi.ButtonRight: mouseButtonSecondary, hostabi.ButtonMiddle: mouseButtonTertiary}[e.Button]
					if !ok {
						continue
					}
					down := e.Action == hostabi.PointerDown
					hostabi.CallGuestWide(buttonFn, []uintptr{env, inputCls, bool2(down), btn}, x, y)
					if debugInput {
						fmt.Printf("input: mouse button %d down=%v at (%.0f,%.0f)\n", btn, down, x, y)
					}
				case hostabi.PointerScroll:
					// AXIS_VSCROLL is positive when scrolling up (away from the user).
					hostabi.CallGuestWide(wheelFn, []uintptr{env, inputCls}, x, y, -e.Scroll)
					if debugInput {
						fmt.Printf("input: wheel %.2f at (%.0f,%.0f)\n", -e.Scroll, x, y)
					}
				}
			case hostabi.InputKey:
				if box, text, changed, enter, consumed := editText(e); consumed {
					cursor := uintptr(len(utf16.Encode(text))) // a Java string index
					if changed || enter {
						hostabi.CallGuestInt8(syncFn, env, glCls, hostabi.NewJavaString(string(text)),
							cursor, 0, 0, 0, 0)
					}
					if enter {
						hostabi.CallGuestInt3(returnFn, env, glCls, box)
						hostabi.CallGuestInt8(textFn, env, glCls, box,
							hostabi.NewJavaString(string(text)), 1, cursor, 0, 0)
					}
					if debugInput {
						fmt.Printf("input: text box %#x text=%q enter=%v\n", box, string(text), enter)
					}
					continue
				}
				repeat := e.Down && held[e.KeyCode]
				held[e.KeyCode] = e.Down
				hostabi.CallGuestInt8(keyFn, env, glCls, bool2(e.Down), uintptr(e.ScanCode),
					uintptr(e.KeyCode), bool2(repeat), 0, 0)
				if debugInput {
					fmt.Printf("input: key %d (scan %d) down=%v repeat=%v\n", e.KeyCode, e.ScanCode, e.Down, repeat)
				}
			}
		}
	}
}

// replayInput plays a scripted input sequence, for testing input without a
// person at the window. The script is a ';'-separated list of steps, each
// "<ms>:<what>" where <ms> is the wait before the step:
//
//	2000:click:640,360     left click at window pixel (640,360)
//	500:drag:100,100,400,100  press, move in steps, release
//	500:key:30             press and release evdev key 30 (A)
func replayInput(script string) {
	if script == "" {
		return
	}
	for _, step := range strings.Split(script, ";") {
		parts := strings.SplitN(strings.TrimSpace(step), ":", 3)
		if len(parts) < 3 {
			fmt.Printf("input: replay: bad step %q\n", step)
			continue
		}
		wait, _ := strconv.Atoi(parts[0])
		time.Sleep(time.Duration(wait) * time.Millisecond)
		var n []float32
		for _, f := range strings.Split(parts[2], ",") {
			v, _ := strconv.ParseFloat(strings.TrimSpace(f), 32)
			n = append(n, float32(v))
		}
		fmt.Printf("input: replay %s %v\n", parts[1], n)
		switch parts[1] {
		case "click":
			if len(n) >= 2 {
				hostabi.InjectPointer(hostabi.PointerMove, 0, n[0], n[1])
				hostabi.InjectPointer(hostabi.PointerDown, hostabi.ButtonLeft, n[0], n[1])
				time.Sleep(80 * time.Millisecond)
				hostabi.InjectPointer(hostabi.PointerUp, hostabi.ButtonLeft, n[0], n[1])
			}
		case "drag":
			if len(n) >= 4 {
				hostabi.InjectPointer(hostabi.PointerDown, hostabi.ButtonLeft, n[0], n[1])
				for i := 1; i <= 10; i++ {
					t := float32(i) / 10
					time.Sleep(16 * time.Millisecond)
					hostabi.InjectPointer(hostabi.PointerMove, 0, n[0]+(n[2]-n[0])*t, n[1]+(n[3]-n[1])*t)
				}
				hostabi.InjectPointer(hostabi.PointerUp, hostabi.ButtonLeft, n[2], n[3])
			}
		case "key":
			if len(n) >= 1 {
				hostabi.InjectKey(int(n[0]), true)
				time.Sleep(60 * time.Millisecond)
				hostabi.InjectKey(int(n[0]), false)
			}
		default:
			fmt.Printf("input: replay: unknown step %q\n", parts[1])
		}
	}
}
