package apk

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"
)

// Verification is the outcome of a successful signature check.
type Verification struct {
	Scheme      int       // APK Signature Scheme: 2 or 3
	CertSHA256  string    // lowercase hex SHA-256 of the signing certificate (DER)
	Subject     string    // certificate subject, human-readable
	Issuer      string    // certificate issuer, human-readable
	Serial      *big.Int  // certificate serial number
	NotBefore   time.Time // certificate validity start
	NotAfter    time.Time // certificate validity end
	ContentHash string    // hash algorithm used to digest the APK contents
}

// Verify checks that the APK at path is whole-file signed with the v2 or v3
// scheme and that its first certificate's SHA-256 digest is in trusted.
func Verify(path string, trusted []string) (*Verification, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return verifyReaderAt(f, fi.Size(), path, trusted)
}

func verifyReaderAt(r io.ReaderAt, size int64, path string, trusted []string) (*Verification, error) {
	if size < 22 {
		return nil, fmt.Errorf("apk: %s is too small to be an APK", path)
	}
	e, err := findEOCD(r, size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if e.cdOffset != -1 && e.cdOffset+e.cdSize != e.offset {
		return nil, fmt.Errorf("apk: %s: ZIP central directory is not immediately followed by the End of Central Directory", path)
	}
	if e.offset+e.size != size {
		return nil, fmt.Errorf("apk: %s: data found after the End of Central Directory record", path)
	}
	layout, err := findSigningBlock(r, e.cdOffset)
	if err != nil {
		if errors.Is(err, errNoSigningBlock) {
			if hasV1Signature(r, size) {
				return nil, fmt.Errorf("apk: %s: signed with the legacy v1 (JAR) scheme only, which cannot prove whole-file authenticity; refusing", path)
			}
			return nil, fmt.Errorf("apk: %s: no cryptographic signature found", path)
		}
		return nil, err
	}

	var lastErr error
	for _, cand := range []struct {
		id     uint32
		scheme int
	}{
		{blockIDSchemeV3, 3},
		{blockIDSchemeV2, 2},
	} {
		data, ok := layout.pairs[cand.id]
		if !ok {
			continue
		}
		signers, err := parseSigners(data, cand.scheme == 3)
		if err != nil {
			lastErr = err
			continue
		}
		allOK := true
		for _, s := range signers {
			if err := verifySigner(s, r, layout, e); err != nil {
				lastErr = err
				allOK = false
				break
			}
		}
		if !allOK {
			continue
		}
		v, err := resultFromSigner(signers[0], cand.scheme)
		if err != nil {
			lastErr = err
			continue
		}
		if !certTrusted(v.CertSHA256, trusted) {
			return nil, fmt.Errorf("apk: %s is signed and unmodified, but not with a trusted Roblox certificate (certificate SHA-256 %s)", path, v.CertSHA256)
		}
		return v, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("apk: %s: no v2/v3 signing block", path)
}

// resultFromSigner derives the report from the first (leaf) certificate.
func resultFromSigner(s *signer, scheme int) (*Verification, error) {
	cert, err := x509.ParseCertificate(s.certificates[0])
	if err != nil {
		return nil, fmt.Errorf("apk: cannot parse signing certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	return &Verification{
		Scheme:      scheme,
		CertSHA256:  hex.EncodeToString(sum[:]),
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		Serial:      cert.SerialNumber,
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		ContentHash: "SHA-256",
	}, nil
}

// hasV1Signature reports whether the archive looks JAR (v1) signed.
func hasV1Signature(r io.ReaderAt, size int64) bool {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		uc := strings.ToUpper(f.Name)
		if strings.HasPrefix(uc, "META-INF/") && (strings.HasSuffix(uc, ".SF") || strings.HasSuffix(uc, ".RSA") || strings.HasSuffix(uc, ".DSA") || strings.HasSuffix(uc, ".EC")) {
			return true
		}
	}
	return false
}
