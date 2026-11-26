package main

import (
	"testing"
	"time"
)

func TestParseSetCookie(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	c, ok := parseSetCookie(".ROBLOSECURITY=_|WARNING:-x|_A=B; domain=.roblox.com; expires=Fri, 26 Nov 2027 12:00:00 GMT; path=/; secure; HttpOnly", now)
	if !ok || c.name != ".ROBLOSECURITY" || c.value != "_|WARNING:-x|_A=B" || c.deleted {
		t.Fatalf("login cookie parsed as %+v", c)
	}
	for _, h := range []string{
		".ROBLOSECURITY=; domain=.roblox.com; path=/",
		".ROBLOSECURITY=x; expires=Thu, 01 Jan 1970 00:00:00 GMT",
		"RBXEventTrackerV2=x; max-age=0",
	} {
		if c, ok := parseSetCookie(h, now); !ok || !c.deleted {
			t.Errorf("%q: not seen as a deletion (%+v)", h, c)
		}
	}
	if _, ok := parseSetCookie("; path=/", now); ok {
		t.Errorf("a header without a name parsed")
	}
}

func TestKeeperTracksLogin(t *testing.T) {
	k := newSessionKeeper(nil)
	k.putLocked("RBXEventTrackerV2", "a")
	k.putLocked(loginCookie, "tok")
	if got := k.headerLocked(); got != "RBXEventTrackerV2=a; .ROBLOSECURITY=tok" {
		t.Fatalf("header = %q", got)
	}
	k.deleteLocked(loginCookie)
	if got := k.headerLocked(); got != "RBXEventTrackerV2=a" || !k.dirty {
		t.Fatalf("after logout: header %q dirty %v", got, k.dirty)
	}
	if v := cookieValue("a=1; .ROBLOSECURITY=x=y", loginCookie); v != "x=y" {
		t.Errorf("cookieValue = %q", v)
	}
	if w := withoutCookie("a=1; .ROBLOSECURITY=x", loginCookie); w != "a=1" {
		t.Errorf("withoutCookie = %q", w)
	}
}
