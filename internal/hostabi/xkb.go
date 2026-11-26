package hostabi

import (
	"os"
	"sync"
	"unsafe"
)

// Keyboard layouts through libxkbcommon, the library Wayland compositors and
// X11 toolkits use to turn key codes into characters. The user's own layout
// (German, French, Dvorak, ...) is used, not a built-in US table:
//
//   - Wayland: the compositor sends the active keymap (wl_keyboard.keymap) and
//     the modifier/layout state (wl_keyboard.modifiers).
//   - X11: the keymap is read from the X server (libxkbcommon-x11), and the
//     state follows each key press and release.
//
// From it each key gets the character it types (with Shift, AltGr, Caps Lock
// and dead keys / compose sequences such as ^ + e = ê) and, for letters and
// digits, the Android key code of what the key is labelled in that layout
// (the German Z key is KEYCODE_Z), as Android's own KeyCharacterMap gives.
// Without libxkbcommon the US mapping in input.go is used.

const (
	xkbKeymapFormatTextV1 = 1
	xkbKeyUp              = 0
	xkbKeyDown            = 1

	xkbComposeNothing   = 0
	xkbComposeComposing = 1
	xkbComposeComposed  = 2
	xkbComposeCancelled = 3
)

var xkb struct {
	mu sync.Mutex

	lib, x11lib uintptr
	ctx         uintptr
	keymap      uintptr
	state       uintptr
	compose     uintptr // compose state, 0 if the locale has no compose table
	trackKeys   bool    // X11: update the state from key events

	fnKeymapNewFromString, fnKeymapUnref, fnStateNew, fnStateUnref  uintptr
	fnUpdateMask, fnUpdateKey, fnGetOneSym, fnGetUTF32, fnGetLayout uintptr
	fnSymsByLevel, fnKeysymToUTF32                                  uintptr
	fnComposeFeed, fnComposeStatus, fnComposeOneSym, fnComposeReset uintptr
}

// xkbInit loads libxkbcommon and creates a context and compose state. It
// reports false (and the US fallback stays in use) if the library is missing.
func xkbInit() bool {
	if xkb.ctx != 0 {
		return true
	}
	lib := hostDlopen("libxkbcommon.so.0", rtldNow|rtldGlobal)
	if lib == 0 {
		return false
	}
	syms := map[string]*uintptr{
		"xkb_keymap_new_from_string":       &xkb.fnKeymapNewFromString,
		"xkb_keymap_unref":                 &xkb.fnKeymapUnref,
		"xkb_state_new":                    &xkb.fnStateNew,
		"xkb_state_unref":                  &xkb.fnStateUnref,
		"xkb_state_update_mask":            &xkb.fnUpdateMask,
		"xkb_state_update_key":             &xkb.fnUpdateKey,
		"xkb_state_key_get_one_sym":        &xkb.fnGetOneSym,
		"xkb_state_key_get_utf32":          &xkb.fnGetUTF32,
		"xkb_state_key_get_layout":         &xkb.fnGetLayout,
		"xkb_keymap_key_get_syms_by_level": &xkb.fnSymsByLevel,
		"xkb_keysym_to_utf32":              &xkb.fnKeysymToUTF32,
	}
	for name, p := range syms {
		if *p = hostDlsym(lib, name); *p == 0 {
			return false
		}
	}
	ctxNew := hostDlsym(lib, "xkb_context_new")
	if ctxNew == 0 {
		return false
	}
	ctx := callHostC(ctxNew, &[9]uintptr{0})
	if ctx == 0 {
		return false
	}
	xkb.lib, xkb.ctx = lib, ctx

	// Dead keys and compose sequences, from the locale's compose table.
	tableNew := hostDlsym(lib, "xkb_compose_table_new_from_locale")
	stateNew := hostDlsym(lib, "xkb_compose_state_new")
	xkb.fnComposeFeed = hostDlsym(lib, "xkb_compose_state_feed")
	xkb.fnComposeStatus = hostDlsym(lib, "xkb_compose_state_get_status")
	xkb.fnComposeOneSym = hostDlsym(lib, "xkb_compose_state_get_one_sym")
	xkb.fnComposeReset = hostDlsym(lib, "xkb_compose_state_reset")
	if tableNew != 0 && stateNew != 0 && xkb.fnComposeFeed != 0 && xkb.fnComposeStatus != 0 &&
		xkb.fnComposeOneSym != 0 && xkb.fnComposeReset != 0 {
		if table := callHostC(tableNew, &[9]uintptr{ctx, CString(composeLocale()), 0}); table != 0 {
			xkb.compose = callHostC(stateNew, &[9]uintptr{table, 0})
		}
	}
	return true
}

// composeLocale is the locale whose compose table is used, as libX11 and
// toolkits choose it.
func composeLocale() string {
	for _, v := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if s := os.Getenv(v); s != "" {
			return s
		}
	}
	return "C"
}

// xkbSetKeymap installs a keymap and a fresh state for it, releasing the old.
func xkbSetKeymap(keymap, state uintptr) {
	xkb.mu.Lock()
	defer xkb.mu.Unlock()
	if xkb.state != 0 {
		callHostC(xkb.fnStateUnref, &[9]uintptr{xkb.state})
	}
	if xkb.keymap != 0 {
		callHostC(xkb.fnKeymapUnref, &[9]uintptr{xkb.keymap})
	}
	xkb.keymap, xkb.state = keymap, state
}

// xkbLoadKeymapText compiles a keymap in XKB text form (what a Wayland
// compositor sends) and makes it current.
func xkbLoadKeymapText(text []byte) bool {
	if !xkbInit() {
		return false
	}
	buf := append(append([]byte(nil), text...), 0)
	keymap := callHostC(xkb.fnKeymapNewFromString, &[9]uintptr{
		xkb.ctx, uintptr(unsafe.Pointer(&buf[0])), xkbKeymapFormatTextV1, 0})
	if keymap == 0 {
		return false
	}
	state := callHostC(xkb.fnStateNew, &[9]uintptr{keymap})
	if state == 0 {
		callHostC(xkb.fnKeymapUnref, &[9]uintptr{keymap})
		return false
	}
	xkbSetKeymap(keymap, state)
	return true
}

// xkbInitX11 reads the X server's keyboard map for the core keyboard.
func xkbInitX11(display uintptr) bool {
	if !xkbInit() {
		return false
	}
	xcbLib := hostDlopen("libX11-xcb.so.1", rtldNow|rtldGlobal)
	x11lib := hostDlopen("libxkbcommon-x11.so.0", rtldNow|rtldGlobal)
	if xcbLib == 0 || x11lib == 0 {
		return false
	}
	getConn := hostDlsym(xcbLib, "XGetXCBConnection")
	setup := hostDlsym(x11lib, "xkb_x11_setup_xkb_extension")
	devID := hostDlsym(x11lib, "xkb_x11_get_core_keyboard_device_id")
	keymapNew := hostDlsym(x11lib, "xkb_x11_keymap_new_from_device")
	stateNew := hostDlsym(x11lib, "xkb_x11_state_new_from_device")
	if getConn == 0 || setup == 0 || devID == 0 || keymapNew == 0 || stateNew == 0 {
		return false
	}
	conn := callHostC(getConn, &[9]uintptr{display})
	if conn == 0 || callHostC(setup, &[9]uintptr{conn, 1, 0, 0, 0, 0, 0, 0}) == 0 {
		return false
	}
	dev := int32(callHostC(devID, &[9]uintptr{conn}))
	if dev < 0 {
		return false
	}
	keymap := callHostC(keymapNew, &[9]uintptr{xkb.ctx, conn, uintptr(dev), 0})
	if keymap == 0 {
		return false
	}
	state := callHostC(stateNew, &[9]uintptr{keymap, conn, uintptr(dev)})
	if state == 0 {
		callHostC(xkb.fnKeymapUnref, &[9]uintptr{keymap})
		return false
	}
	xkb.x11lib = x11lib
	xkbSetKeymap(keymap, state)
	xkb.trackKeys = true
	return true
}

// xkbUpdateMask applies the compositor's modifier and layout state (Wayland).
func xkbUpdateMask(depressed, latched, locked, group uintptr) {
	xkb.mu.Lock()
	defer xkb.mu.Unlock()
	if xkb.state == 0 {
		return
	}
	callHostC(xkb.fnUpdateMask, &[9]uintptr{xkb.state,
		uintptr(uint32(depressed)), uintptr(uint32(latched)), uintptr(uint32(locked)),
		0, 0, uintptr(uint32(group))})
}

// xkbTranslate looks up an evdev key in the current layout. It returns the
// character the press types (0 for none, for a key release, or while a dead
// key / compose sequence is still pending) and the key's unshifted keysym in
// the active layout (for its Android key code). ok is false without a keymap.
func xkbTranslate(evdev int, down bool) (ch rune, base uint32, ok bool) {
	xkb.mu.Lock()
	defer xkb.mu.Unlock()
	if xkb.state == 0 {
		return 0, 0, false
	}
	key := uintptr(evdev + 8) // xkb key codes are evdev + 8
	sym := uint32(callHostC(xkb.fnGetOneSym, &[9]uintptr{xkb.state, key}))

	layout := callHostC(xkb.fnGetLayout, &[9]uintptr{xkb.state, key})
	var syms uintptr
	if int32(callHostC(xkb.fnSymsByLevel, &[9]uintptr{xkb.keymap, key, uintptr(uint32(layout)), 0,
		uintptr(unsafe.Pointer(&syms))})) > 0 && syms != 0 {
		base = *(*uint32)(unsafe.Pointer(syms))
	}

	if down {
		ch = rune(uint32(callHostC(xkb.fnGetUTF32, &[9]uintptr{xkb.state, key})))
		if xkb.compose != 0 && sym != 0 {
			callHostC(xkb.fnComposeFeed, &[9]uintptr{xkb.compose, uintptr(sym)})
			switch int32(callHostC(xkb.fnComposeStatus, &[9]uintptr{xkb.compose})) {
			case xkbComposeComposing:
				ch = 0
			case xkbComposeComposed:
				s := callHostC(xkb.fnComposeOneSym, &[9]uintptr{xkb.compose})
				ch = rune(uint32(callHostC(xkb.fnKeysymToUTF32, &[9]uintptr{s})))
				callHostC(xkb.fnComposeReset, &[9]uintptr{xkb.compose})
			case xkbComposeCancelled:
				ch = 0
				callHostC(xkb.fnComposeReset, &[9]uintptr{xkb.compose})
			}
		}
	}
	if xkb.trackKeys {
		dir := uintptr(xkbKeyUp)
		if down {
			dir = xkbKeyDown
		}
		callHostC(xkb.fnUpdateKey, &[9]uintptr{xkb.state, key, dir})
	}
	return ch, base, true
}

// keysymAndroid maps a layout's unshifted keysym for a letter or digit to the
// Android key code labelled with it (KEYCODE_A..Z, KEYCODE_0..9); 0 otherwise.
func keysymAndroid(sym uint32) int {
	switch {
	case sym >= 'a' && sym <= 'z':
		return 29 + int(sym-'a')
	case sym >= 'A' && sym <= 'Z':
		return 29 + int(sym-'A')
	case sym >= '0' && sym <= '9':
		return 7 + int(sym-'0')
	}
	return 0
}
