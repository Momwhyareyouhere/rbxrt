package apk

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Environment variable that names a JSON file listing trusted certificate
// SHA-256 hashes (lowercase hex, e.g. ["44932ea3..."]). When unset, the
// built-in list is used.
const EnvTrustedCerts = "RBXRT_TRUSTED_CERTIFICATES"

// DefaultRobloxCertificates are the SHA-256 digests of the DER X.509
// certificates that Roblox Corporation signs com.roblox.client with.
//
// These are public facts anyone can recompute from any genuine Roblox APK
// (`openssl dgst -sha256` over the embedded certificate). The values below were
// cross-checked against the trust file of the independent open-source runtime
// project "Cordial" (packaging/trust/roblox-signing-certificates.json), which
// records that they were read from a genuine base.apk (Roblox 2.734.0.917,
// x86_64). Roblox has rotated its signing key at least once and both digests
// appear on current builds. Roblox may rotate again; when verification starts
// failing on an APK that OEMs are serving, add the new digest here — taking a
// digest from an APK you trust is deliberately a manual, auditable decision.
var DefaultRobloxCertificates = []string{
	"44932ea35a17a267372d71b54d1a0cb3da0dca5113e94406ae2fe18090ba1477",
	"2bebd189e8d3106401347056c93d045b61e20e22d0c3cbed85474aeb00a3d12a",
}

// TrustedCertificates returns the built-in Roblox certificate list, or the
// JSON array named by $RBXRT_TRUSTED_CERTIFICATES when that is set.
func TrustedCertificates() ([]string, error) {
	p := os.Getenv(EnvTrustedCerts)
	if p == "" {
		return append([]string(nil), DefaultRobloxCertificates...), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("apk: reading %s: %w", p, err)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("apk: %s is not a JSON list of certificate hashes: %w", p, err)
	}
	for i := range list {
		list[i] = strings.ToLower(strings.TrimSpace(list[i]))
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("apk: trusted certificate list in %s is empty", p)
	}
	return list, nil
}

// certTrusted reports whether a certificate digest is in the trusted set.
func certTrusted(sha string, trusted []string) bool {
	for _, t := range trusted {
		if strings.EqualFold(t, sha) {
			return true
		}
	}
	return false
}
