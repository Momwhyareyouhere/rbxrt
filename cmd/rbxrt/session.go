package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rbxrt/internal/hostabi"
)

// Staying signed in across restarts.
//
// The engine keeps its HTTP cookies (including .ROBLOSECURITY, the Roblox
// login) in memory only. On Android the Java app persists them: it keeps them
// in android.webkit.CookieManager and, at the next launch, hands them back with
// NativeSettingsInterface.nativeSetMultipleCookies(url,
// CookieManager.getCookie(url)) for url = "https://www.roblox.com" (recovered
// from the dex, class b1),
// then starts the Lua app as that user.
//
// rbxrt does the same with a file in the instance: every few seconds it reads
// the engine's cookies (nativeGetCookiesForDomain) and, when they changed,
// writes them to <instance>/data/session.json (mode 0600: it holds the login
// token). At launch it restores them, asks Roblox which account the login
// belongs to (users.roblox.com/v1/users/authenticated, as the app does), and
// boots signed in as that account. An expired or revoked login boots signed
// out. Logging out in the app removes the cookie from the engine, and the next
// save records that.
//
// RBXRT_NO_SESSION=1 neither restores nor saves.

const (
	sessionURL      = "https://www.roblox.com"
	sessionInterval = 5 * time.Second
	loginCookie     = ".ROBLOSECURITY"
)

type savedSession struct {
	URL     string    `json:"url"`
	Cookies string    `json:"cookies"` // "name=value; name2=value2", as CookieManager.getCookie
	Saved   time.Time `json:"saved"`
}

// sessionUser is the account a restored login belongs to.
type sessionUser struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

func sessionEnabled() bool { return os.Getenv("RBXRT_NO_SESSION") == "" }

func sessionPath() string { return filepath.Join(rbxrtDataDir(), "session.json") }

func loadSession() (savedSession, bool) {
	var s savedSession
	b, err := os.ReadFile(sessionPath())
	if err != nil || json.Unmarshal(b, &s) != nil || s.Cookies == "" {
		return savedSession{}, false
	}
	if s.URL == "" {
		s.URL = sessionURL
	}
	return s, true
}

// writeSession saves the cookies atomically, readable by the owner only.
func writeSession(s savedSession) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := sessionPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sessionPath())
}

// cookieValue returns the value of name in a "a=1; b=2" cookie string.
func cookieValue(cookies, name string) string {
	for _, c := range strings.Split(cookies, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(c), "=")
		if ok && k == name {
			return v
		}
	}
	return ""
}

// netscapeToHeader converts the engine's cookie dump (curl's Netscape
// format: domain, subdomains, path, secure, expiry, name, value, tab-separated,
// one per line, "#HttpOnly_" before the domain of HttpOnly cookies) into the
// "a=1; b=2" form nativeSetMultipleCookies takes. Expired cookies are dropped.
func netscapeToHeader(dump string, now time.Time) string {
	var out []string
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 || f[5] == "" {
			continue
		}
		var exp int64
		fmt.Sscan(f[4], &exp)
		if exp != 0 && exp < now.Unix() {
			continue
		}
		out = append(out, f[5]+"="+f[6])
	}
	return strings.Join(out, "; ")
}

// withoutCookie drops one cookie from a "a=1; b=2" string.
func withoutCookie(cookies, name string) string {
	var keep []string
	for _, c := range strings.Split(cookies, ";") {
		c = strings.TrimSpace(c)
		if k, _, _ := strings.Cut(c, "="); c != "" && k != name {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, "; ")
}

// errLoggedOut means Roblox rejected the saved login (expired or revoked).
var errLoggedOut = fmt.Errorf("the saved login is no longer valid")

// authenticatedUser asks Roblox which account a login cookie belongs to.
func authenticatedUser(token string) (sessionUser, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://users.roblox.com/v1/users/authenticated", nil)
	if err != nil {
		return sessionUser{}, err
	}
	req.Header.Set("Cookie", loginCookie+"="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sessionUser{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return sessionUser{}, errLoggedOut
	case resp.StatusCode != http.StatusOK:
		return sessionUser{}, fmt.Errorf("users.roblox.com answered %s", resp.Status)
	}
	var u sessionUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return sessionUser{}, err
	}
	if u.ID == 0 {
		return sessionUser{}, errLoggedOut
	}
	return u, nil
}

// seedSessionUser makes the Java user interface and the app-start parameters
// describe the signed-in account, as the Java app does after a login.
func seedSessionUser(u sessionUser) {
	str := hostabi.NewJavaString
	hostabi.SetFakeMethodResult(classUser, "getUserId", uintptr(u.ID))
	hostabi.SetFakeMethodResult(classUser, "getUsername", str(u.Name))
	hostabi.SetFakeMethodResult(classUser, "getDisplayName", str(u.DisplayName))
	hostabi.SetFakeMethodResult(classStartApp, "appUserId", uintptr(u.ID))
	hostabi.SetFakeMethodResult(classStartApp, "username", str(u.Name))
	hostabi.SetFakeMethodResult(classStartGame, "userId", uintptr(u.ID))
	hostabi.SetFakeMethodResult(classStartGame, "username", str(u.Name))
}

// sessionKeeper restores the saved cookies into the engine and keeps the
// file up to date. callSettings calls a NativeSettingsInterface native with
// (env, class, args...) and returns its result.
type sessionKeeper struct {
	call     func(name string, args ...uintptr) uintptr
	last     string
	lastSave time.Time
}

// restore puts the saved cookies back and reports the signed-in account, if
// the saved login is still valid.
func (k *sessionKeeper) restore() (sessionUser, bool) {
	s, ok := loadSession()
	if !ok {
		return sessionUser{}, false
	}
	cookies := s.Cookies
	var user sessionUser
	signedIn := false
	if token := cookieValue(cookies, loginCookie); token != "" {
		u, err := authenticatedUser(token)
		switch {
		case err == nil:
			user, signedIn = u, true
			fmt.Printf("session: signed in as %s (%d)\n", u.Name, u.ID)
		case err == errLoggedOut:
			fmt.Printf("session: %v; starting signed out\n", err)
			cookies = withoutCookie(cookies, loginCookie)
		default:
			// Offline or Roblox unreachable: keep the login for next time,
			// but the account cannot be identified, so start signed out.
			fmt.Printf("session: could not check the saved login (%v); starting signed out\n", err)
		}
	}
	if cookies != "" {
		k.call("nativeSetMultipleCookies", hostabi.NewJavaString(s.URL), hostabi.NewJavaString(cookies))
	}
	if signedIn {
		k.call("nativeSetUserId", hostabi.NewJavaString(fmt.Sprint(user.ID)))
	}
	k.last = s.Cookies
	return user, signedIn
}

// save reads the engine's cookies and writes them if they changed. It runs on
// the main loop at most every sessionInterval.
func (k *sessionKeeper) save(force bool) {
	if !force && time.Since(k.lastSave) < sessionInterval {
		return
	}
	k.lastSave = time.Now()
	h := k.call("nativeGetCookiesForDomain", hostabi.NewJavaString(sessionURL))
	dump, ok := hostabi.JavaStringValue(h)
	if !ok {
		return
	}
	cookies := netscapeToHeader(dump, time.Now())
	// An empty jar is never written over a saved one: signing out removes
	// the login cookie but leaves Roblox's other cookies, so "nothing at all"
	// means the engine has not loaded them yet, not that the user left.
	if cookies == "" || cookies == k.last {
		return
	}
	if err := writeSession(savedSession{URL: sessionURL, Cookies: cookies, Saved: time.Now()}); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: session: save: %v\n", err)
		return
	}
	switch had, has := cookieValue(k.last, loginCookie) != "", cookieValue(cookies, loginCookie) != ""; {
	case has && !had:
		fmt.Printf("session: login saved; you will stay signed in\n")
	case had && !has:
		fmt.Printf("session: signed out; saved login removed\n")
	}
	k.last = cookies
}
