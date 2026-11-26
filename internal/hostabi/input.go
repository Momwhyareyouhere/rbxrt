package hostabi

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Input from the host window, queued for the launch driver to hand to the
// engine the way Android's framework would: as MotionEvents and KeyEvents
// delivered to GameActivity's onTouchEventNative / onKeyDownNative /
// onKeyUpNative on the main thread.
//
// The window backends (gfx_wayland.go, gfx_x11.go) push events while they
// dispatch their own event streams; the driver drains them each main-loop
// iteration with PollInput. Coordinates are window pixels, which are also the
// engine's surface pixels (the surface is created at the window's size).

// InputKind distinguishes pointer from keyboard events.
type InputKind int

const (
	InputPointer InputKind = iota // a mouse button or motion, in window pixels
	InputKey                      // a key press or release
)

// Pointer actions.
const (
	PointerMove   = iota // motion, with or without a button held
	PointerDown          // a button was pressed
	PointerUp            // a button was released
	PointerScroll        // wheel; Scroll holds the amount (positive = down)
	PointerLeave         // the pointer left the window
)

// Mouse buttons (Linux evdev codes, as both Wayland and our X11 mapping use).
const (
	ButtonLeft   = 0x110
	ButtonRight  = 0x111
	ButtonMiddle = 0x112
)

// InputEvent is one event from the host window.
type InputEvent struct {
	Kind InputKind
	Time time.Duration // CLOCK_MONOTONIC-based time since boot of the event

	// Pointer.
	Action int
	Button int // evdev button code for PointerDown/PointerUp
	X, Y   float32
	Scroll float32

	// Key.
	Down     bool
	KeyCode  int  // Android KEYCODE_*
	ScanCode int  // evdev key code
	Meta     int  // Android META_* state
	Unicode  rune // character the key produces with Meta (0 if none)
}

var input struct {
	mu     sync.Mutex
	events []InputEvent
	x, y   float32 // last pointer position
	meta   int     // current Android meta state

	// Client-side key repeat (Wayland leaves it to the client; X11 repeats
	// on its own): the held key and when it next repeats.
	repeatKey  int
	repeatNext time.Time
}

const (
	keyRepeatDelay = 600 * time.Millisecond
	keyRepeatEvery = 40 * time.Millisecond
)

// startRepeat / stopRepeat track the key a Wayland keyboard holds down.
func startRepeat(evdev int) {
	if metaForKey(evdevToAndroid(evdev)) != 0 {
		return // modifiers do not repeat
	}
	input.mu.Lock()
	input.repeatKey, input.repeatNext = evdev, time.Now().Add(keyRepeatDelay)
	input.mu.Unlock()
}

func stopRepeat(evdev int) {
	input.mu.Lock()
	if input.repeatKey == evdev || evdev == 0 {
		input.repeatKey = 0
	}
	input.mu.Unlock()
}

// repeatKeys queues the repeats due for a held key.
func repeatKeys() {
	input.mu.Lock()
	k, now := input.repeatKey, time.Now()
	n := 0
	for k != 0 && !now.Before(input.repeatNext) && n < 10 {
		input.repeatNext = input.repeatNext.Add(keyRepeatEvery)
		n++
	}
	input.mu.Unlock()
	for i := 0; i < n; i++ {
		keyEvent(k, true)
	}
}

func pushInput(e InputEvent) {
	if e.Time == 0 {
		e.Time = monotonicNow()
	}
	input.mu.Lock()
	if len(input.events) < 4096 { // a stalled main loop must not grow this forever
		input.events = append(input.events, e)
	}
	input.mu.Unlock()
}

// PollInput returns the events queued since the last call, oldest first. It
// also gives the window backend a chance to read new events from the display
// server (X11 is read here; Wayland is also read before each frame).
func PollInput() []InputEvent {
	pumpWindow()
	repeatKeys()
	input.mu.Lock()
	ev := input.events
	input.events = nil
	input.mu.Unlock()
	return ev
}

// pumpMu serializes reading the display connection: the main loop
// (PollInput) and the render thread (eglSwapBuffers) both pump it.
var pumpMu sync.Mutex

// pumpWindow reads and dispatches pending display-server events without
// blocking.
func pumpWindow() {
	if !gfx.ready {
		return
	}
	pumpMu.Lock()
	defer pumpMu.Unlock()
	switch gfx.platform {
	case "x11":
		x11Pump()
	case "wayland":
		waylandPump()
	}
}

func monotonicNow() time.Duration {
	var ts syscall.Timespec
	syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0) // CLOCK_MONOTONIC
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
}

func pointerEvent(action, button int, x, y float32) {
	input.mu.Lock()
	input.x, input.y = x, y
	input.mu.Unlock()
	pushInput(InputEvent{Kind: InputPointer, Action: action, Button: button, X: x, Y: y})
}

func pointerAt() (float32, float32) {
	input.mu.Lock()
	defer input.mu.Unlock()
	return input.x, input.y
}

// keyEvent queues a key press or release given its evdev code. Modifier keys
// also update the meta state the following keys report.
func keyEvent(evdev int, down bool) {
	kc := evdevToAndroid(evdev)
	if kc == 0 {
		return
	}
	input.mu.Lock()
	if bit := metaForKey(kc); bit != 0 {
		if down {
			input.meta |= bit
		} else {
			input.meta &^= bit
		}
	}
	meta := input.meta
	input.mu.Unlock()
	pushInput(InputEvent{Kind: InputKey, Down: down, KeyCode: kc, ScanCode: evdev,
		Meta: meta, Unicode: keyRune(evdev, meta)})
}

// setCapsLock records Caps Lock from the display server's lock state.
func setCapsLock(on bool) {
	input.mu.Lock()
	if on {
		input.meta |= metaCapsLockOn
	} else {
		input.meta &^= metaCapsLockOn
	}
	input.mu.Unlock()
}

// Android KeyEvent META_* bits.
const (
	metaShiftOn    = 0x1
	metaAltOn      = 0x2
	metaAltLeft    = 0x10
	metaAltRight   = 0x20
	metaShiftLeft  = 0x40
	metaShiftRight = 0x80
	metaCtrlOn     = 0x1000
	metaCtrlLeft   = 0x2000
	metaCtrlRight  = 0x4000
	metaMetaOn     = 0x10000
	metaMetaLeft   = 0x20000
	metaMetaRight  = 0x40000
	metaCapsLockOn = 0x100000
)

func metaForKey(kc int) int {
	switch kc {
	case 59:
		return metaShiftOn | metaShiftLeft
	case 60:
		return metaShiftOn | metaShiftRight
	case 57:
		return metaAltOn | metaAltLeft
	case 58:
		return metaAltOn | metaAltRight
	case 113:
		return metaCtrlOn | metaCtrlLeft
	case 114:
		return metaCtrlOn | metaCtrlRight
	case 117:
		return metaMetaOn | metaMetaLeft
	case 118:
		return metaMetaOn | metaMetaRight
	}
	return 0
}

// evdevAndroid maps Linux evdev key codes (linux/input-event-codes.h) to
// Android KEYCODE_* values (android/keycodes.h).
var evdevAndroid = map[int]int{
	1: 111,                                                              // ESC -> ESCAPE
	2: 8, 3: 9, 4: 10, 5: 11, 6: 12, 7: 13, 8: 14, 9: 15, 10: 16, 11: 7, // 1..9, 0
	12: 69, 13: 70, 14: 67, 15: 61, // MINUS, EQUALS, BACKSPACE -> DEL, TAB
	16: 45, 17: 51, 18: 33, 19: 46, 20: 48, 21: 53, 22: 49, 23: 37, 24: 43, 25: 44, // Q..P
	26: 71, 27: 72, 28: 66, 29: 113, // [ ] ENTER LCTRL
	30: 29, 31: 47, 32: 32, 33: 34, 34: 35, 35: 36, 36: 38, 37: 39, 38: 40, // A..L
	39: 74, 40: 75, 41: 68, 42: 59, 43: 73, // ; ' ` LSHIFT backslash
	44: 54, 45: 52, 46: 31, 47: 50, 48: 30, 49: 42, 50: 41, // Z..M
	51: 55, 52: 56, 53: 76, 54: 60, 55: 155, 56: 57, 57: 62, 58: 115, // , . / RSHIFT KP* LALT SPACE CAPS
	59: 131, 60: 132, 61: 133, 62: 134, 63: 135, 64: 136, 65: 137, 66: 138, 67: 139, 68: 140, // F1..F10
	69: 143, 70: 116, // NUMLOCK, SCROLLLOCK
	71: 151, 72: 152, 73: 153, 74: 156, 75: 148, 76: 149, 77: 150, 78: 157, // KP7 KP8 KP9 KP- KP4 KP5 KP6 KP+
	79: 145, 80: 146, 81: 147, 82: 144, 83: 158, // KP1 KP2 KP3 KP0 KP.
	87: 141, 88: 142, // F11 F12
	96: 160, 97: 114, 98: 154, 100: 58, // KPENTER RCTRL KP/ RALT
	102: 122, 103: 19, 104: 92, 105: 21, 106: 22, 107: 123, 108: 20, 109: 93, // HOME UP PGUP LEFT RIGHT END DOWN PGDN
	110: 124, 111: 112, 119: 121, // INSERT, DELETE -> FORWARD_DEL, PAUSE -> BREAK
	125: 117, 126: 118, 127: 82, // LMETA RMETA COMPOSE -> MENU
}

func evdevToAndroid(evdev int) int { return evdevAndroid[evdev] }

// keyRune is the character a key produces on a US layout. The engine's text
// boxes read KeyEvent.getUnicodeChar; a full keymap (xkb) is not consulted.
func keyRune(evdev, meta int) rune {
	const lower = "1234567890-=" // evdev 2..13
	const upper = "!@#$%^&*()_+"
	shift := meta&metaShiftOn != 0
	if meta&(metaCtrlOn|metaAltOn|metaMetaOn) != 0 {
		return 0
	}
	switch {
	case evdev >= 2 && evdev <= 13:
		if shift {
			return rune(upper[evdev-2])
		}
		return rune(lower[evdev-2])
	case evdev == 57:
		return ' '
	case evdev == 28 || evdev == 96:
		return '\n'
	case evdev == 15:
		return '\t'
	}
	rows := map[int][2]string{16: {"qwertyuiop[]", "QWERTYUIOP{}"},
		30: {"asdfghjkl;'`", "ASDFGHJKL:\"~"}, 44: {"zxcvbnm,./", "ZXCVBNM<>?"}}
	for start, r := range rows {
		if evdev >= start && evdev < start+len(r[0]) {
			c := rune(r[0][evdev-start])
			if c >= 'a' && c <= 'z' {
				if shift != (meta&metaCapsLockOn != 0) {
					c -= 'a' - 'A'
				}
				return c
			}
			if shift {
				c = rune(r[1][evdev-start])
			}
			return c
		}
	}
	if evdev == 43 {
		if shift {
			return '|'
		}
		return '\\'
	}
	return 0
}

// InjectPointer queues a synthetic pointer event as if it came from the
// window (used by RBXRT_INPUT_REPLAY to exercise input without a person).
func InjectPointer(action, button int, x, y float32) { pointerEvent(action, button, x, y) }

// InjectKey queues a synthetic key press or release by evdev code.
func InjectKey(evdev int, down bool) { keyEvent(evdev, down) }
