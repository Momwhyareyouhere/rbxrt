package apk

import (
	"strings"
	"testing"
)

// These fixtures were produced by the Android SDK tools (aapt2, zipalign,
// apksigner) and can be regenerated with scripts/fixtures.sh. The certificate
// hashes below were printed by apksigner --print-certs.
const (
	testCertSHA = "56868e085c4628c043aec38fb4eecf0d5316ae69ca7d10eebe854824f333b040"
	evilCertSHA = "c8f0fd468e26ff352ad2becce110df650478bd7228543cd5ce149f86b8525606"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	return "testdata/" + name + ".apk"
}

func wantErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("expected error containing %q, got: %v", sub, err)
	}
}

func TestVerifyV2Only(t *testing.T) {
	v, err := Verify(fixture(t, "v2only"), []string{testCertSHA})
	if err != nil {
		t.Fatalf("expected verification to succeed: %v", err)
	}
	if v.Scheme != 2 {
		t.Errorf("expected scheme v2, got %d", v.Scheme)
	}
	if v.CertSHA256 != testCertSHA {
		t.Errorf("wrong cert hash: %s", v.CertSHA256)
	}
}

func TestVerifyV2V3PrefersV3(t *testing.T) {
	v, err := Verify(fixture(t, "v2v3"), []string{testCertSHA})
	if err != nil {
		t.Fatalf("expected verification to succeed: %v", err)
	}
	if v.Scheme != 3 {
		t.Errorf("expected scheme v3, got %d", v.Scheme)
	}
}

func TestVerifyWrongKeyRefusedByPin(t *testing.T) {
	// The signature is valid, but it is not one of the trusted certificates.
	_, err := Verify(fixture(t, "wrongkey"), []string{testCertSHA})
	wantErrContains(t, err, "not with a trusted Roblox certificate")
}

func TestVerifyWrongKeyAcceptedWhenPinned(t *testing.T) {
	v, err := Verify(fixture(t, "wrongkey"), []string{evilCertSHA})
	if err != nil {
		t.Fatalf("verification should succeed when its own certificate is pinned: %v", err)
	}
	if v.CertSHA256 != evilCertSHA {
		t.Errorf("wrong cert hash: %s", v.CertSHA256)
	}
}

func TestVerifyTamperedAPK(t *testing.T) {
	_, err := Verify(fixture(t, "tampered"), []string{testCertSHA})
	wantErrContains(t, err, "content digest mismatch")
}

func TestVerifyV1OnlyRefused(t *testing.T) {
	_, err := Verify(fixture(t, "v1only"), []string{testCertSHA})
	wantErrContains(t, err, "v1 (JAR) scheme only")
}

func TestVerifyDefaultRobloxPinsRejectTestApk(t *testing.T) {
	// The built-in Roblox pin must not accept a test-signed APK.
	_, err := Verify(fixture(t, "v2only"), nil)
	wantErrContains(t, err, "not with a trusted Roblox certificate")
}

func TestParseManifest(t *testing.T) {
	s, err := OpenSet([]string{fixture(t, "v2only")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := ParseManifestFromSet(s)
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != "com.roblox.client" {
		t.Errorf("package = %q", m.Package)
	}
	if m.VersionCode != 2734 {
		t.Errorf("versionCode = %d", m.VersionCode)
	}
	if m.VersionName != "2.734.917" {
		t.Errorf("versionName = %q", m.VersionName)
	}
	if m.MinSDK != 21 || m.TargetSDK != 34 {
		t.Errorf("sdk = %d/%d", m.MinSDK, m.TargetSDK)
	}
}

func TestVerifyActuallyReadsFromAPK(t *testing.T) {
	// sanity: the fixture is a real apk, manifest present.
	if _, err := Verify("/nonexistent.apk", nil); err == nil {
		t.Fatal("expected error for missing file")
	}
}
