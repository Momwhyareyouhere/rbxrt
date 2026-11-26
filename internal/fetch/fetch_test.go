package fetch

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testCertSHA is the certificate the internal/apk fixtures are signed with.
const testCertSHA = "56868e085c4628c043aec38fb4eecf0d5316ae69ca7d10eebe854824f333b040"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "apk", "testdata", name+".apk"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mirror serves body as the mirror would; a negative claimLen advertises a
// Content-Length that does not match (a truncated transfer).
func mirror(t *testing.T, body []byte, claimLen int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != userAgent {
			t.Errorf("User-Agent = %q, want the generic %q", ua, userAgent)
		}
		w.Header().Set("Content-Disposition", `attachment; filename="Roblox_test.apk"`)
		if claimLen > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(claimLen))
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func download(t *testing.T, url, dest string) (*Result, error) {
	t.Helper()
	return Download(context.Background(), dest, Options{URL: url, Trusted: []string{testCertSHA}})
}

func TestDownloadAcceptsGenuineAPK(t *testing.T) {
	body := fixture(t, "v2v3")
	dest := filepath.Join(t.TempDir(), "base.apk")
	r, err := download(t, mirror(t, body, len(body)), dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if r.Path != dest || r.Size != int64(len(body)) || r.Verification == nil || r.MirrorName != "Roblox_test.apk" {
		t.Fatalf("result = %+v", r)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, body) {
		t.Fatal("dest does not hold the downloaded APK")
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// A download that fails verification must leave an existing APK untouched.
func TestDownloadRejectsAndKeepsPrevious(t *testing.T) {
	good := fixture(t, "v2only")
	for name, tc := range map[string]struct {
		body     []byte
		claimLen int
		want     string
	}{
		"tampered":  {fixture(t, "tampered"), 0, "not a genuine Roblox APK"},
		"wrong key": {fixture(t, "wrongkey"), 0, "not a genuine Roblox APK"},
		"v1 only":   {fixture(t, "v1only"), 0, "not a genuine Roblox APK"},
		"not a zip": {[]byte("<html>blocked</html>"), 0, "not an APK"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "base.apk")
			if err := os.WriteFile(dest, good, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := download(t, mirror(t, tc.body, tc.claimLen), dest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got, _ := os.ReadFile(dest); !bytes.Equal(got, good) {
				t.Fatal("a rejected download replaced the existing APK")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Fatalf("temporary files left behind: %v", entries)
			}
		})
	}
}

func TestDownloadRejectsTruncatedTransfer(t *testing.T) {
	body := fixture(t, "v2v3")
	// Advertise more bytes than are sent: the server closes early.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+1000))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "base.apk")
	if _, err := Download(context.Background(), dest, Options{URL: srv.URL, Trusted: []string{testCertSHA}}); err == nil {
		t.Fatal("a truncated transfer was accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a truncated transfer created the destination")
	}
}

func TestDownloadRejectsXAPKBundle(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"manifest.json", "com.roblox.client.apk", "config.x86_64.apk"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	_, err := download(t, mirror(t, buf.Bytes(), buf.Len()), filepath.Join(t.TempDir(), "base.apk"))
	if err == nil || !strings.Contains(err.Error(), "XAPK") {
		t.Fatalf("err = %v, want an XAPK rejection", err)
	}
}
