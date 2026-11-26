package main

import (
	"os"
	"testing"
)

func TestAuthenticatedUserRejectsBadToken(t *testing.T) {
	if os.Getenv("RBXRT_NET_TEST") == "" {
		t.Skip("set RBXRT_NET_TEST=1 to contact users.roblox.com")
	}
	u, err := authenticatedUser("rbxrt-invalid-test-token")
	t.Logf("user=%+v err=%v", u, err)
	if err != errLoggedOut {
		t.Fatalf("err = %v, want errLoggedOut", err)
	}
}
