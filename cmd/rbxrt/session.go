package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rbxrt/internal/hostabi"
)

// Staying signed in across restarts.
//
// The engine keeps its HTTP cookies (including .ROBLOSECURITY, the Roblox
// login) in memory only; on Android the Java app persists them, and rbxrt
// plays that part. Recovered from the APK's dex:
//
//   - While running, the app registers a handler with the engine's cookie
//     protocol (JNICookieProtocol.updateOnSetCookieHandler). The engine then
//     calls handler.onSetCookie(String[] setCookieHeaders, String url) for
//     every cookie Roblox sets or deletes, including the login, and the app
//     stores them in android.webkit.CookieManager.
//   - At the next launch it hands them back with
//     NativeSettingsInterface.nativeSetMultipleCookies(url,
//     CookieManager.getCookie(url)), url = "https://www.roblox.com", and starts
//     the Lua app as the signed-in user.
//
// rbxrt registers its own handler, keeps the cookies in
// <instance>/data/session.json (mode 0600: it holds the login token), and
// restores them the same way. Before starting signed in it asks Roblox which
// account the login belongs to (users.roblox.com/v1/users/authenticated). An
// expired or revoked login starts signed out. Signing out makes Roblox delete
// the cookie, which reaches the handler and the file like any other change.
//
// RBXRT_NO_SESSION=1 neither restores nor saves.

const (
	sessionURL  = "https://www.roblox.com"
	loginCookie = ".ROBLOSECURITY"

	classCookieProtocolJNI = "com/roblox/universalapp/cookie/JNICookieProtocol"
	classSetCookieHandler  = "com/roblox/universalapp/cookie/CookieProtocol$OnSetCookieHandlerImpl"
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

// debugSession logs cookie names and the shape of what the engine reports,
// never values (RBXRT_DEBUG_SESSION).
var debugSession = os.Getenv("RBXRT_DEBUG_SESSION") != ""

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

// --- cookie strings ---------------------------------------------------------

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

// setCookie is one Set-Cookie header, reduced to what persisting needs.
type setCookie struct {
	name, value string
	deleted     bool // expired, Max-Age <= 0, or emptied
	attrs       []string
}

// parseSetCookie reads "name=value; Domain=...; Expires=...; Max-Age=...".
func parseSetCookie(h string, now time.Time) (setCookie, bool) {
	parts := strings.Split(h, ";")
	name, value, ok := strings.Cut(strings.TrimSpace(parts[0]), "=")
	if !ok || name == "" {
		return setCookie{}, false
	}
	c := setCookie{name: name, value: value, deleted: value == ""}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		c.attrs = append(c.attrs, strings.ToLower(k))
		switch strings.ToLower(k) {
		case "max-age":
			var n int64
			if _, err := fmt.Sscan(v, &n); err == nil && n <= 0 {
				c.deleted = true
			}
		case "expires":
			if t, err := http.ParseTime(v); err == nil && !t.After(now) {
				c.deleted = true
			}
		}
	}
	return c, true
}

// --- account lookup ---------------------------------------------------------

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

// --- the keeper -------------------------------------------------------------

// sessionKeeper holds the cookies the engine reported, restores them at
// launch and writes them to session.json whenever they change.
type sessionKeeper struct {
	// call calls a NativeSettingsInterface native with (env, class, args...).
	call func(name string, args ...uintptr) uintptr

	mu      sync.Mutex
	names   []string          // in the order first seen
	values  map[string]string // name -> value
	dirty   bool
	written string // the cookie string last written
}

func newSessionKeeper(call func(name string, args ...uintptr) uintptr) *sessionKeeper {
	return &sessionKeeper{call: call, values: map[string]string{}}
}

// headerLocked renders the cookies as "a=1; b=2". k.mu must be held.
func (k *sessionKeeper) headerLocked() string {
	var out []string
	for _, n := range k.names {
		if v, ok := k.values[n]; ok {
			out = append(out, n+"="+v)
		}
	}
	return strings.Join(out, "; ")
}

func (k *sessionKeeper) putLocked(name, value string) {
	if _, ok := k.values[name]; !ok {
		k.names = append(k.names, name)
	}
	if k.values[name] != value {
		k.values[name] = value
		k.dirty = true
	}
}

func (k *sessionKeeper) deleteLocked(name string) {
	if _, ok := k.values[name]; ok {
		delete(k.values, name)
		k.dirty = true
	}
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
	k.mu.Lock()
	for _, c := range strings.Split(s.Cookies, ";") {
		if n, v, ok := strings.Cut(strings.TrimSpace(c), "="); ok && n != "" {
			k.putLocked(n, v)
		}
	}
	if cookieValue(cookies, loginCookie) == "" {
		k.deleteLocked(loginCookie) // rejected by Roblox: forget it
	}
	k.dirty = cookies != s.Cookies
	k.written = s.Cookies
	k.mu.Unlock()

	if cookies != "" {
		k.call("nativeSetMultipleCookies", hostabi.NewJavaString(s.URL), hostabi.NewJavaString(cookies))
	}
	if signedIn {
		k.call("nativeSetUserId", hostabi.NewJavaString(fmt.Sprint(user.ID)))
	}
	return user, signedIn
}

// onSetCookie receives the engine's cookie reports (a String[] of Set-Cookie
// headers). It runs on whichever engine thread made the request.
func (k *sessionKeeper) onSetCookie(arr uintptr) {
	now := time.Now()
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, h := range hostabi.ObjectArrayElements(arr) {
		text, ok := hostabi.JavaStringValue(h)
		if !ok {
			continue
		}
		c, ok := parseSetCookie(text, now)
		if !ok {
			continue
		}
		if debugSession {
			fmt.Printf("session: debug engine set cookie %s (deleted=%v, attributes %v)\n", c.name, c.deleted, c.attrs)
		}
		had := k.values[loginCookie] != ""
		if c.deleted {
			k.deleteLocked(c.name)
		} else {
			k.putLocked(c.name, c.value)
		}
		if c.name == loginCookie {
			switch has := k.values[loginCookie] != ""; {
			case has && !had:
				fmt.Printf("session: login received; you will stay signed in\n")
			case had && !has:
				fmt.Printf("session: signed out; saved login removed\n")
			}
		}
	}
}

// register hands the engine a cookie handler, as the app does at start-up.
// exports resolves the engine's JNI natives.
func (k *sessionKeeper) register(exports map[string]uintptr, env uintptr) {
	fn, ok := exports["Java_com_roblox_universalapp_cookie_JNICookieProtocol_updateOnSetCookieHandler"]
	if !ok {
		fmt.Printf("session: this engine has no cookie protocol; logins are not saved\n")
		return
	}
	hostabi.SetFakeMethodArgFunc(classSetCookieHandler, "onSetCookie", func(arr uintptr) uintptr {
		k.onSetCookie(arr)
		return 0
	})
	protocol := hostabi.NewFakeObject(classCookieProtocolJNI)
	handler := hostabi.NewFakeObject(classSetCookieHandler)
	hostabi.CallGuestInt3(fn, env, protocol, handler)
}

// watchLogin hooks the engine's login report to the Java app
// (NativeHelper.gameActivity_onDidLogInReceived) for a log line, and, under
// RBXRT_DEBUG_SESSION, to describe what the report holds (keys only).
func (k *sessionKeeper) watchLogin() {
	hostabi.SetFakeMethodArgFunc(classNativeHelper, "gameActivity_onDidLogInReceived", func(arg uintptr) uintptr {
		fmt.Printf("session: the app reports a login\n")
		if debugSession {
			t, _ := hostabi.JavaStringValue(arg)
			fmt.Printf("session: debug login report: %s\n", describeSecretText(t))
		}
		return 0
	})
}

// describeSecretText says what a string holds without revealing it: the keys
// of a JSON object, or just its length.
func describeSecretText(t string) string {
	var m map[string]any
	if json.Unmarshal([]byte(t), &m) == nil {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		return fmt.Sprintf("JSON keys %v", keys)
	}
	return fmt.Sprintf("%d bytes", len(t))
}

// save writes the cookies if they changed. It runs on the main loop.
func (k *sessionKeeper) save() {
	k.mu.Lock()
	if !k.dirty {
		k.mu.Unlock()
		return
	}
	cookies := k.headerLocked()
	k.dirty = false
	same := cookies == k.written
	k.mu.Unlock()
	if same {
		return
	}
	if cookies == "" {
		// Nothing left to keep (signed out and no other cookies).
		os.Remove(sessionPath())
	} else if err := writeSession(savedSession{URL: sessionURL, Cookies: cookies, Saved: time.Now()}); err != nil {
		fmt.Fprintf(os.Stderr, "rbxrt: session: save: %v\n", err)
		return
	}
	if debugSession {
		k.mu.Lock()
		fmt.Printf("session: debug saved cookies %v\n", k.names)
		k.mu.Unlock()
	}
	k.mu.Lock()
	k.written = cookies
	k.mu.Unlock()
}
