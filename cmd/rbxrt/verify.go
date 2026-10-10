package main

import (
	"fmt"
	"os"

	"rbxrt/internal/apk"
)

func cmdVerify(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rbxrt verify <apk>")
	}
	trusted, err := apk.TrustedCertificates()
	if err != nil {
		return err
	}
	v, err := apk.Verify(args[0], trusted)
	if err != nil {
		return err
	}
	fmt.Printf("OK %s\n", args[0])
	fmt.Printf("  scheme:      APK Signature Scheme v%d\n", v.Scheme)
	fmt.Printf("  content hash: %s (whole-file integrity OK)\n", v.ContentHash)
	fmt.Printf("  cert sha256: %s\n", v.CertSHA256)
	fmt.Printf("  subject:     %s\n", v.Subject)
	if v.Issuer != "" && v.Issuer != v.Subject {
		fmt.Printf("  issuer:      %s\n", v.Issuer)
	}
	fmt.Printf("  serial:      %s\n", v.Serial)
	fmt.Printf("  valid:       %s .. %s\n", v.NotBefore.Format("2006-01-02"), v.NotAfter.Format("2006-01-02"))
	return nil
}

func cmdInspect(args []string) error {
	if len(args) != 1 {
		// Inspect is verify plus archive contents; reuse the verify path.
		return cmdVerify(args)
	}
	return cmdVerify(args)
}

var _ = os.Args
