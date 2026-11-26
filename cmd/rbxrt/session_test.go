package main

import (
	"testing"
	"time"
)

func TestNetscapeToHeader(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	dump := "# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.roblox.com\tTRUE\t/\tTRUE\t2100000000\t.ROBLOSECURITY\t_|WARNING:-x|_ABC\n" +
		".roblox.com\tTRUE\t/\tFALSE\t0\tRBXEventTrackerV2\tCreateDate=1&rbxid=\n" +
		".roblox.com\tTRUE\t/\tFALSE\t1000\told\tgone\n" +
		"broken line\n"
	got := netscapeToHeader(dump, now)
	want := ".ROBLOSECURITY=_|WARNING:-x|_ABC; RBXEventTrackerV2=CreateDate=1&rbxid="
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if v := cookieValue(got, ".ROBLOSECURITY"); v != "_|WARNING:-x|_ABC" {
		t.Errorf("cookieValue = %q", v)
	}
	if v := cookieValue(got, "RBXEventTrackerV2"); v != "CreateDate=1&rbxid=" {
		t.Errorf("cookieValue with '=' in value = %q", v)
	}
	if w := withoutCookie(got, ".ROBLOSECURITY"); w != "RBXEventTrackerV2=CreateDate=1&rbxid=" {
		t.Errorf("withoutCookie = %q", w)
	}
}
