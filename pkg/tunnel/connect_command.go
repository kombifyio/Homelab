package tunnel

import (
	"net"
	"net/url"
	"strings"
)

// ConnectCommand carries the resolved control-plane origin, never the pairing.
// Private bridges keep their pinned installer rather than this unpinned path.
func ConnectCommand(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return ""
	}
	ip := net.ParseIP(parsed.Hostname())
	loopback := strings.EqualFold(parsed.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return ""
	}
	if ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return ""
	}
	return "sudo stackkit connect --server " + shQuote(parsed.Scheme+"://"+parsed.Host)
}
