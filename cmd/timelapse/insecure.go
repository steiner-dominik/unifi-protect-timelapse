package main

import (
	"crypto/tls"
	"net/http"
)

// insecureTransport skips certificate verification, for UniFi OS consoles that
// present a self-signed certificate. It is only used when the operator opts in
// with PROTECT_INSECURE_TLS=true.
func insecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in for self-signed console certificates
	}
}
