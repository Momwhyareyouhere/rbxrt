package hostabi

import (
	"testing"
	"time"
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
