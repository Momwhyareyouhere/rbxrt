// Package fetch downloads the Roblox Android client from a public mirror and
// accepts it only once it is proven genuine.
//
// The mirror is untrusted transport. A download is written to a temporary
// file next to its destination, checked against the advertised length, and
// then verified with internal/apk: whole-file integrity under the APK
// Signature Scheme v2/v3 (the signed content digests cover every byte) and the
// signing certificate pinned to Roblox's. Only a file that passes is renamed
// into place, so a failed, truncated or tampered download never replaces a
// good APK.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rbxrt/internal/apk"
)

// Package is the Roblox client's Android package name.
const Package = "com.roblox.client"

// MirrorURL returns the APKPure download URL for the latest build of pkg. The
// "nc" parameter asks for a build carrying the x86_64 native libraries; the
// mirror serves the universal APK, which has every ABI.
func MirrorURL(pkg string) string {
	return "https://d.apkpure.net/b/APK/" + pkg + "?version=latest&nc=x86_64"
}

// Result describes an accepted download.
type Result struct {
	Path         string            // where the verified APK now is
	Size         int64             // bytes
	SHA256       string            // hex digest of the whole file
	VersionName  string            // from the APK's manifest, e.g. "2.748.1527"
	VersionCode  int64             // from the APK's manifest
	MirrorName   string            // the file name the mirror offered
	Verification *apk.Verification // signature scheme and certificate
}

// Options controls a download.
type Options struct {
	// URL overrides MirrorURL(Package).
	URL string
	// Client is the HTTP client; nil uses one with a generous timeout.
	Client *http.Client
	// Progress, if set, is called as bytes arrive (total is -1 if unknown).
	Progress func(done, total int64)
	// Package is the expected package name; empty means Package.
	Package string
	// Trusted overrides the pinned certificate list (tests only).
	Trusted []string
}

// userAgent identifies the tool generically; nothing about the user is sent.
const userAgent = "rbxrt"

// Download fetches the latest Roblox APK to dest. dest is replaced only by a
// verified APK.
func Download(ctx context.Context, dest string, o Options) (*Result, error) {
	pkg := o.Package
	if pkg == "" {
		pkg = Package
	}
	url := o.URL
	if url == "" {
		url = MirrorURL(pkg)
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	trusted := o.Trusted
	if trusted == nil {
		var err error
		if trusted, err = apk.TrustedCertificates(); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*.apk")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		tmp.Close()
		if !keep {
			os.Remove(tmpPath)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: %s: HTTP %d", url, resp.StatusCode)
	}

	h := sha256.New()
	var body io.Reader = resp.Body
	if o.Progress != nil {
		body = &progressReader{r: resp.Body, total: resp.ContentLength, fn: o.Progress}
	}
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return nil, fmt.Errorf("download: truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	if err := checkSingleAPK(tmpPath); err != nil {
		return nil, err
	}
	v, err := apk.Verify(tmpPath, trusted)
	if err != nil {
		return nil, fmt.Errorf("downloaded file is not a genuine Roblox APK: %w", err)
	}
	m, err := readManifest(tmpPath)
	if err != nil {
		return nil, err
	}
	if m.Package != pkg {
		return nil, fmt.Errorf("downloaded APK is %q, not %s", m.Package, pkg)
	}

	if err := os.Chmod(tmpPath, 0o644); err != nil { // CreateTemp makes it 0600
		return nil, err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return nil, err
	}
	keep = true
	return &Result{
		Path:         dest,
		Size:         n,
		SHA256:       hex.EncodeToString(h.Sum(nil)),
		VersionName:  m.VersionName,
		VersionCode:  m.VersionCode,
		MirrorName:   dispositionName(resp),
		Verification: v,
	}, nil
}

// checkSingleAPK rejects an XAPK bundle (a zip of split APKs), which the
// mirror serves for some apps: rbxrt runs a single universal APK.
func checkSingleAPK(path string) error {
	a, err := apk.OpenArchive(path)
	if err != nil {
		return fmt.Errorf("downloaded file is not an APK: %w", err)
	}
	defer a.Close()
	if a.Has("AndroidManifest.xml") {
		return nil
	}
	for _, e := range a.Entries() {
		if strings.HasSuffix(e, ".apk") {
			return errors.New("the mirror served an XAPK bundle (split APKs); rbxrt needs the universal APK")
		}
	}
	return errors.New("downloaded archive has no AndroidManifest.xml")
}

func readManifest(path string) (*apk.Manifest, error) {
	a, err := apk.OpenArchive(path)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	data, err := a.Read("AndroidManifest.xml")
	if err != nil {
		return nil, err
	}
	return apk.ParseManifest(data)
}

func dispositionName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		return params["filename"]
	}
	return ""
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	fn    func(done, total int64)
	last  time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if now := time.Now(); err != nil || now.Sub(p.last) > 250*time.Millisecond {
		p.last = now
		p.fn(p.done, p.total)
	}
	return n, err
}
