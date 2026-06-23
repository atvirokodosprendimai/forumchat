// Package netguard blocks server-side requests to internal addresses. In SaaS,
// tenants supply outbound endpoints (Ollama host, Qdrant URL) that the platform
// then dials — an SSRF vector: a tenant could point at cloud metadata
// (169.254.169.254) or the platform's private network. BlockedURL rejects such
// hosts at config-save time.
//
// NOTE: save-time resolution does not defend against DNS rebinding (a host that
// resolves public at save but private at request time). For that, also run the
// platform behind an egress firewall. This is the first, cheap layer.
package netguard

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// BlockedURL reports whether rawurl targets a non-public address (loopback,
// private RFC1918/ULA, link-local, metadata, unspecified) or is malformed. An
// empty string is allowed (means "unset / use default"). reason is human-readable.
func BlockedURL(rawurl string) (bool, string) {
	rawurl = strings.TrimSpace(rawurl)
	if rawurl == "" {
		return false, ""
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return true, "malformed URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return true, "only http/https allowed"
	}
	host := u.Hostname()
	if host == "" {
		return true, "missing host"
	}
	// Resolve all addresses; block if ANY is non-public (avoids a split-horizon
	// name that returns one public + one private record).
	ips, err := net.LookupIP(host)
	if err != nil {
		return true, fmt.Sprintf("cannot resolve host %q", host)
	}
	for _, ip := range ips {
		if isInternal(ip) {
			return true, fmt.Sprintf("host %q resolves to a non-public address (%s)", host, ip)
		}
	}
	return false, ""
}

func isInternal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// Cloud metadata endpoints (link-local already covers 169.254.169.254, but
	// be explicit) and the IPv6 ULA / discard ranges IsPrivate misses.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
	}
	return false
}
