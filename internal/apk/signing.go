package apk

import (
	"bytes"
	"crypto"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
)

// APK Signature Scheme block IDs inside the APK signing block.
const (
	blockIDSchemeV2 = 0x7109871a
	blockIDSchemeV3 = 0xf05368c0
)

const chunkDigestSectionSize = 1 << 20 // 1 MiB, fixed by the APK Signature Scheme.

// digestRec is one (algorithm ID, digest) pair inside a signer's signed data.
type digestRec struct {
	algorithmID uint32
	digest      []byte
}

// sigRec is one (algorithm ID, signature) pair of a signer.
type sigRec struct {
	algorithmID uint32
	signature   []byte
}

// signer is one parsed signer from a v2 or v3 block.
type signer struct {
	signedData   []byte // exact bytes the signature is computed over
	digests      []digestRec
	certificates [][]byte // DER encoded X.509
	minSdk       int64    // v3 only
	maxSdk       int64    // v3 only
	publicKey    []byte   // SubjectPublicKeyInfo DER
	signatures   []sigRec
}

// parsedPubKey wraps a parsed public key.
type parsedPubKey struct {
	rsa   *rsa.PublicKey
	ecdsa *ecdsa.PublicKey
	dsa   *dsa.PublicKey
}

func pkFrom(spki []byte) (*parsedPubKey, error) {
	key, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, fmt.Errorf("apk: bad public key: %w", err)
	}
	pk := &parsedPubKey{}
	switch k := key.(type) {
	case *rsa.PublicKey:
		pk.rsa = k
	case *ecdsa.PublicKey:
		pk.ecdsa = k
	case *dsa.PublicKey:
		pk.dsa = k
	default:
		return nil, fmt.Errorf("apk: unsupported public key type %T", key)
	}
	return pk, nil
}

// sigAlg describes one signature algorithm we can verify locally.
type sigAlg struct {
	id          uint32
	contentHash string // "SHA-256" or "SHA-512"
	verify      func(pk *parsedPubKey, digest, sig []byte) error
}

var supportedAlgs = []sigAlg{
	// RSASSA-PSS, MGF1 with the same hash, salt = hash size.
	{id: 0x0101, contentHash: "SHA-256", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.rsa == nil {
			return errors.New("apk: RSA-PSS signature but key is not RSA")
		}
		h := sha256.Sum256(d)
		return rsa.VerifyPSS(k.rsa, crypto.SHA256, h[:], s, &rsa.PSSOptions{SaltLength: 32})
	}},
	{id: 0x0102, contentHash: "SHA-512", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.rsa == nil {
			return errors.New("apk: RSA-PSS signature but key is not RSA")
		}
		h := sha512.Sum512(d)
		return rsa.VerifyPSS(k.rsa, crypto.SHA512, h[:], s, &rsa.PSSOptions{SaltLength: 64})
	}},
	// RSASSA-PKCS1-v1_5.
	{id: 0x0103, contentHash: "SHA-256", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.rsa == nil {
			return errors.New("apk: RSA signature but key is not RSA")
		}
		h := sha256.Sum256(d)
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], s)
	}},
	{id: 0x0104, contentHash: "SHA-512", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.rsa == nil {
			return errors.New("apk: RSA signature but key is not RSA")
		}
		h := sha512.Sum512(d)
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA512, h[:], s)
	}},
	// ECDSA over P-256/P-384/P-521.
	{id: 0x0201, contentHash: "SHA-256", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.ecdsa == nil {
			return errors.New("apk: ECDSA signature but key is not EC")
		}
		h := sha256.Sum256(d)
		if !ecdsa.VerifyASN1(k.ecdsa, h[:], s) {
			return errors.New("apk: ECDSA signature did not verify")
		}
		return nil
	}},
	{id: 0x0202, contentHash: "SHA-512", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.ecdsa == nil {
			return errors.New("apk: ECDSA signature but key is not EC")
		}
		h := sha512.Sum512(d)
		if !ecdsa.VerifyASN1(k.ecdsa, h[:], s) {
			return errors.New("apk: ECDSA signature did not verify")
		}
		return nil
	}},
	// DSA with SHA-256.
	{id: 0x0301, contentHash: "SHA-256", verify: func(k *parsedPubKey, d, s []byte) error {
		if k.dsa == nil {
			return errors.New("apk: DSA signature but key is not DSA")
		}
		h := sha256.Sum256(d)
		var sig struct{ R, S *big.Int }
		if _, err := asn1.Unmarshal(s, &sig); err != nil {
			return fmt.Errorf("apk: bad DSA signature: %w", err)
		}
		if !dsa.Verify(k.dsa, h[:], sig.R, sig.S) {
			return errors.New("apk: DSA signature did not verify")
		}
		return nil
	}},
}

func findAlg(id uint32) (sigAlg, bool) {
	for _, a := range supportedAlgs {
		if a.id == id {
			return a, true
		}
	}
	return sigAlg{}, false
}

// ---- length-prefixed parsing ----

type byteReader struct {
	b   []byte
	pos int
}

func (r *byteReader) remaining() int { return len(r.b) - r.pos }

func (r *byteReader) u32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *byteReader) prefixed() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if int(n) < 0 || r.remaining() < int(n) {
		return nil, fmt.Errorf("apk: length-prefixed field (%d bytes) exceeds block", n)
	}
	out := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

func (r *byteReader) skipPrefixed() error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	if int(n) < 0 || r.remaining() < int(n) {
		return fmt.Errorf("apk: length-prefixed field (%d bytes) exceeds block", n)
	}
	r.pos += int(n)
	return nil
}

// parseSigners parses a v2 or v3 signer list. The scheme decides whether the
// min/max SDK fields are present.
//
// On the wire: the block value is {u32 seqLen}{signers}, and each signer is
// itself {u32 len}{signer}. (Verified byte-for-byte against AOSP
// ApkSigningBlockUtils/V2SchemeVerifier.)
func parseSigners(data []byte, v3 bool) ([]*signer, error) {
	r := &byteReader{b: data}
	seq, err := r.prefixed() // strip the sequence length
	if err != nil {
		return nil, fmt.Errorf("apk: signer sequence: %w", err)
	}
	outer := &byteReader{b: seq}
	var out []*signer
	for outer.remaining() > 0 {
		sb, err := outer.prefixed() // strip one signer's length
		if err != nil {
			return nil, err
		}
		s, err := parseOneSigner(sb, v3)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("apk: signing block has no signers")
	}
	return out, nil
}

func parseOneSigner(data []byte, v3 bool) (*signer, error) {
	r := &byteReader{b: data}
	sd, err := r.prefixed()
	if err != nil {
		return nil, fmt.Errorf("apk: signed data: %w", err)
	}
	s := &signer{signedData: sd}
	if v3 {
		if m1, err := r.u32(); err != nil {
			return nil, fmt.Errorf("apk: v3 minSdk: %w", err)
		} else {
			s.minSdk = int64(m1)
		}
		if m2, err := r.u32(); err != nil {
			return nil, fmt.Errorf("apk: v3 maxSdk: %w", err)
		} else {
			s.maxSdk = int64(m2)
		}
	}
	// Contents of the signed data.
	sdR := &byteReader{b: sd}
	dg, err := sdR.prefixed()
	if err != nil {
		return nil, fmt.Errorf("apk: digests: %w", err)
	}
	dr := &byteReader{b: dg}
	for dr.remaining() > 0 {
		rec, err := dr.prefixed() // one digest record
		if err != nil {
			return nil, fmt.Errorf("apk: digest record: %w", err)
		}
		rr := &byteReader{b: rec}
		alg, err := rr.u32()
		if err != nil {
			return nil, err
		}
		d, err := rr.prefixed()
		if err != nil {
			return nil, err
		}
		s.digests = append(s.digests, digestRec{algorithmID: alg, digest: d})
	}
	cl, err := sdR.prefixed()
	if err != nil {
		return nil, fmt.Errorf("apk: certificates: %w", err)
	}
	cr := &byteReader{b: cl}
	for cr.remaining() > 0 {
		cert, err := cr.prefixed()
		if err != nil {
			return nil, err
		}
		s.certificates = append(s.certificates, cert)
	}
	if len(s.certificates) == 0 {
		return nil, errors.New("apk: signer has no certificates")
	}
	if v3 {
		// min/max SDK are also inside signed data, after certificates.
		if _, err := sdR.u32(); err != nil {
			return nil, err
		}
		if _, err := sdR.u32(); err != nil {
			return nil, err
		}
	}
	if err := sdR.skipPrefixed(); err != nil { // additional attributes
		return nil, fmt.Errorf("apk: additional attributes: %w", err)
	}
	// Signatures and public key follow signed data in the signer.
	sl, err := r.prefixed()
	if err != nil {
		return nil, fmt.Errorf("apk: signatures: %w", err)
	}
	sr := &byteReader{b: sl}
	for sr.remaining() > 0 {
		rec, err := sr.prefixed() // one signature record
		if err != nil {
			return nil, fmt.Errorf("apk: signature record: %w", err)
		}
		rr := &byteReader{b: rec}
		alg, err := rr.u32()
		if err != nil {
			return nil, err
		}
		sig, err := rr.prefixed()
		if err != nil {
			return nil, err
		}
		s.signatures = append(s.signatures, sigRec{algorithmID: alg, signature: sig})
	}
	pk, err := r.prefixed()
	if err != nil {
		return nil, fmt.Errorf("apk: public key: %w", err)
	}
	s.publicKey = pk
	return s, nil
}

// ---- APK content digest (the part that must be byte-exact) ----

func newContentHash(name string) hash.Hash {
	switch name {
	case "SHA-256":
		return sha256.New()
	case "SHA-512":
		return sha512.New()
	}
	return nil
}

// computeContentDigest hashes the three protected sections (contents, central
// directory, EOCD) with the spec's chunked scheme:
//
//	chunk digest  = H( 0xa5 || u32le(chunkLen) || chunk )
//	content digest= H( 0x5a || u32le(totalChunkCount) || chunkDigests... )
//
// The EOCD's central directory offset field is rewritten to the signing block
// offset so the digest is independent of the block's size.
func computeContentDigest(r io.ReaderAt, layout *signingBlockLayout, e *eocd, contentHash string) ([]byte, error) {
	newH := newContentHash(contentHash)
	if newH == nil {
		return nil, fmt.Errorf("apk: unsupported content hash %q", contentHash)
	}
	sections := []struct {
		off, size int64
		patchCD   bool
	}{
		{0, layout.start, false},
		{e.cdOffset, e.cdSize, false},
		{e.offset, e.size, true},
	}
	totalChunks := int64(0)
	for _, s := range sections {
		totalChunks += (s.size + chunkDigestSectionSize - 1) / chunkDigestSectionSize
	}
	if totalChunks > 0xffffffff {
		return nil, fmt.Errorf("apk: APK too large to digest (%d chunks)", totalChunks)
	}

	// top must be its own instance: it accumulates the 0x5a header and every
	// chunk digest, while each chunk gets a fresh hash. Reusing one instance for
	// both (as an earlier version did) lets the per-chunk Reset() wipe top's
	// accumulated state.
	top := newH
	top.Write([]byte{0x5a})
	var u32buf [4]byte
	binary.LittleEndian.PutUint32(u32buf[:], uint32(totalChunks))
	top.Write(u32buf[:])

	var prefix [5]byte
	prefix[0] = 0xa5
	chunk := make([]byte, chunkDigestSectionSize)

	for _, sec := range sections {
		remaining := sec.size
		pos := sec.off
		for remaining > 0 {
			n := remaining
			if n > chunkDigestSectionSize {
				n = chunkDigestSectionSize
			}
			var data []byte
			if sec.patchCD {
				buf := make([]byte, n)
				if _, err := io.ReadFull(io.NewSectionReader(r, pos, n), buf); err != nil {
					return nil, err
				}
				binary.LittleEndian.PutUint32(buf[16:20], uint32(layout.start))
				data = buf
			} else {
				data = chunk[:n]
				if _, err := io.ReadFull(io.NewSectionReader(r, pos, n), data); err != nil {
					return nil, err
				}
			}
			binary.LittleEndian.PutUint32(prefix[1:], uint32(n))
			ch := newContentHash(contentHash) // fresh hash for this chunk
			ch.Write(prefix[:])
			ch.Write(data)
			_, err := top.Write(ch.Sum(nil))
			if err != nil {
				return nil, err
			}
			remaining -= n
			pos += n
		}
	}
	return top.Sum(nil), nil
}

// ---- per-signer verification ----

// verifySigner checks one signer: the signature over its signed data, the APK
// content digest, and the signer public key matching the first certificate.
func verifySigner(s *signer, r io.ReaderAt, layout *signingBlockLayout, e *eocd) error {
	if !sameAlgOrder(s.digests, s.signatures) {
		return errors.New("apk: signature algorithm list mismatch (tampered)")
	}
	pk, err := pkFrom(s.publicKey)
	if err != nil {
		return err
	}
	for _, alg := range supportedAlgs {
		var sig []byte
		for _, sr := range s.signatures {
			if sr.algorithmID == alg.id {
				sig = sr.signature
				break
			}
		}
		if sig == nil {
			continue
		}
		if err := alg.verify(pk, s.signedData, sig); err != nil {
			continue // try the next supported algorithm
		}
		var stored []byte
		for _, d := range s.digests {
			if d.algorithmID == alg.id {
				stored = d.digest
				break
			}
		}
		if stored == nil {
			continue
		}
		computed, err := computeContentDigest(r, layout, e, alg.contentHash)
		if err != nil {
			return err
		}
		if !bytes.Equal(computed, stored) {
			return fmt.Errorf("apk: APK content digest mismatch (expected %s digest)", alg.contentHash)
		}
		cert, err := x509.ParseCertificate(s.certificates[0])
		if err != nil {
			return fmt.Errorf("apk: bad certificate: %w", err)
		}
		spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
		if err != nil {
			return fmt.Errorf("apk: cannot marshal certificate key: %w", err)
		}
		if !bytes.Equal(spki, s.publicKey) {
			return errors.New("apk: signer public key does not match its certificate (tampered)")
		}
		// remember which algorithm verified, for reporting
		return nil
	}
	return errors.New("apk: no supported, valid signature found for the signer")
}

func sameAlgOrder(ds []digestRec, ss []sigRec) bool {
	if len(ds) != len(ss) {
		return false
	}
	for i := range ds {
		if ds[i].algorithmID != ss[i].algorithmID {
			return false
		}
	}
	return true
}
