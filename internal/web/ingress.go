package web

import (
	"net/http"
	"strings"
)

// ingressHeader is set by the Home Assistant Supervisor on every proxied
// request and carries the generated path the add-on is served under.
const ingressHeader = "X-Ingress-Path"

// basePath returns the prefix the app is being served under, or "" when it is
// reached directly. Every asset reference and API call in the frontend is built
// from this, which is what lets one build work both standalone and behind
// Home Assistant ingress.
//
// The value is sanitised rather than trusted verbatim: it is echoed into the
// HTML, so anything that is not a simple absolute path is discarded.
func basePath(r *http.Request) string {
	raw := r.Header.Get(ingressHeader)
	if raw == "" {
		return ""
	}
	return sanitizeBasePath(raw)
}

// sanitizeBasePath accepts only a plain absolute path: it must start with a
// single slash, must not start a scheme or protocol-relative URL, must not
// traverse upwards, and may contain only characters that are safe both in a URL
// and inside an HTML attribute.
func sanitizeBasePath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, "/")

	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	if strings.Contains(raw, "..") {
		return ""
	}

	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '-', r == '_', r == '.', r == '~':
		default:
			return ""
		}
	}
	return raw
}
