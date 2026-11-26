package hostabi

import (
	"testing"
	"time"
	"unsafe"
)

func drainInput() []InputEvent {
	input.mu.Lock()
	defer input.mu.Unlock()
	ev := input.events
	input.events = nil
	return ev
}

func TestKeyMapping(t *testing.T) {
	drainInput()
	keyEvent(30, true) // A
	keyEvent(42, true) // left shift
	keyEvent(30, true)
	keyEvent(42, false)
	keyEvent(14, true) // backspace
	ev := drainInput()
	if len(ev) != 5 {
		t.Fatalf("got %d events, want 5", len(ev))
	}
	if ev[0].KeyCode != 29 || ev[0].Unicode != 'a' {
		t.Errorf("A: keycode %d char %q, want 29 'a'", ev[0].KeyCode, ev[0].Unicode)
	}
	if ev[2].Unicode != 'A' || ev[2].Meta&metaShiftOn == 0 {
		t.Errorf("shift+A: char %q meta %#x, want 'A' with shift", ev[2].Unicode, ev[2].Meta)
	}
	if ev[4].KeyCode != 67 || ev[4].Meta != 0 {
		t.Errorf("backspace: keycode %d meta %#x, want 67 and no modifiers", ev[4].KeyCode, ev[4].Meta)
	}
}

func TestKeyRepeat(t *testing.T) {
	drainInput()
	startRepeat(14)
	repeatKeys()
	if n := len(drainInput()); n != 0 {
		t.Fatalf("repeated %d times before the delay", n)
	}
	input.mu.Lock()
	input.repeatNext = time.Now().Add(-2*keyRepeatEvery + time.Millisecond)
	input.mu.Unlock()
	repeatKeys()
	if ev := drainInput(); len(ev) != 2 || !ev[0].Down || ev[0].KeyCode != 67 {
		t.Fatalf("after the delay: %+v, want two backspace presses", ev)
	}
	stopRepeat(14)
	input.mu.Lock()
	input.repeatNext = time.Now().Add(-time.Second)
	input.mu.Unlock()
	repeatKeys()
	if n := len(drainInput()); n != 0 {
		t.Fatalf("repeated %d times after release", n)
	}
	startRepeat(42) // shift
	input.mu.Lock()
	k := input.repeatKey
	input.mu.Unlock()
	if k != 0 {
		t.Fatalf("modifier key set to repeat")
	}
}

// TestGermanLayout loads the German keymap (from the system's
// xkeyboard-config) and checks what keys type through it.
func TestGermanLayout(t *testing.T) {
	if !hostDlAvailable() || !xkbInit() {
		t.Skip("libxkbcommon not available")
	}
	fromNames := hostDlsym(xkb.lib, "xkb_keymap_new_from_names")
	names := [5]uintptr{CString("evdev"), CString("pc105"), CString("de"), CString(""), CString("")}
	keymap := callHostC(fromNames, &[9]uintptr{xkb.ctx, uintptr(unsafe.Pointer(&names[0])), 0})
	if keymap == 0 {
		t.Skip("German keymap not available")
	}
	xkbSetKeymap(keymap, callHostC(xkb.fnStateNew, &[9]uintptr{keymap}))
	xkb.trackKeys = true
	defer func() { xkbSetKeymap(0, 0); xkb.trackKeys = false }()

	type1 := func(keys ...int) rune {
		drainInput()
		for _, k := range keys {
			keyEvent(k, true)
		}
		for i := len(keys) - 1; i >= 0; i-- {
			keyEvent(keys[i], false)
		}
		for _, e := range drainInput() {
			if e.Down && e.Unicode != 0 {
				return e.Unicode
			}
		}
		return 0
	}
	cases := []struct {
		name string
		keys []int
		want rune
	}{
		{"Y position types z", []int{21}, 'z'},
		{"Z position types y", []int{44}, 'y'},
		{"semicolon position types ö", []int{39}, 'ö'},
		{"minus position types ß", []int{12}, 'ß'},
		{"shift+7 types /", []int{42, 8}, '/'},
		{"AltGr+Q types @", []int{100, 16}, '@'},
		{"102nd key types <", []int{86}, '<'},
		{"Ctrl+A types nothing", []int{29, 30}, 0},
	}
	for _, c := range cases {
		if got := type1(c.keys...); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// The key labelled Z on a German keyboard is KEYCODE_Z.
	drainInput()
	keyEvent(21, true)
	keyEvent(21, false)
	if ev := drainInput(); len(ev) == 0 || ev[0].KeyCode != 54 {
		t.Errorf("German Z key: keycode %v, want 54 (KEYCODE_Z)", ev)
	}

	// Dead key: ^ then e is ê (needs the locale's compose table).
	if xkb.compose != 0 {
		drainInput()
		keyEvent(41, true)
		keyEvent(41, false)
		keyEvent(18, true)
		keyEvent(18, false)
		var got []rune
		for _, e := range drainInput() {
			if e.Unicode != 0 {
				got = append(got, e.Unicode)
			}
		}
		if string(got) != "ê" {
			t.Errorf("dead ^ + e typed %q, want \"ê\"", string(got))
		}
	} else {
		t.Log("no compose table for the locale; dead keys not tested")
	}
}
