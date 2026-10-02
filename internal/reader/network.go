package reader

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

var (
	errUnsafeTarget      = errors.New("URL resolves to a non-public address")
	errUnverifiedXStatus = errors.New("redirected X status pages require verified post text")
	nonPublicRanges      = prefixes(
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "100.100.0.0/16",
		"127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	)
)

// safeTransport 禁用代理；包括 t.co 在内的每个重定向目标都会重做校验，并直接连接已校验 IP 防止 DNS rebinding。
func safeTransport(opts Options) http.RoundTripper {
	dialer := &net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}
	allowed := parseAllowedNonPublicCIDRs(opts.AllowedNonPublicCIDRs)
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublic(ctx, network, address, allowed, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
		},
		TLSHandshakeTimeout:   opts.Timeout,
		ResponseHeaderTimeout: opts.Timeout,
		IdleConnTimeout:       90 * time.Second,
	}
}

// newReaderWithTransport is the package-private fixture seam; production always uses safeTransport.
func newReaderWithTransport(opts Options, transport http.RoundTripper) *Reader {
	opts = defaults(opts)
	allowed := parseAllowedNonPublicCIDRs(opts.AllowedNonPublicCIDRs)
	return &Reader{
		opts:                  opts,
		allowedNonPublicCIDRs: allowed,
		client: &http.Client{
			Timeout:   opts.Timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				// 每次重定向都重新执行 scheme/凭据/目标主机校验；后续拨号仍走固定 IP transport。
				normalized, err := NormalizeURL(req.URL.String())
				if err != nil {
					return err
				}
				u, _ := url.Parse(normalized)
				if isXStatusURL(u) {
					return errUnverifiedXStatus
				}
				if ip := net.ParseIP(u.Hostname()); ip != nil && !isAllowedTargetIP(ip, "", allowed) {
					return errUnsafeTarget
				}
				req.URL = u
				req.Host = ""
				return nil
			},
		},
	}
}

// dialPublic 拒绝混合公网/非公网地址的 DNS 答案，并直接连接已校验 IP，避免二次解析更换目标。
func dialPublic(ctx context.Context, network, address string, allowed []netip.Prefix, lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips := []net.IPAddr{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, net.IPAddr{IP: ip})
	} else {
		ips, err = lookup(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("host resolved to no addresses")
	}
	for _, addr := range ips {
		if !isAllowedTargetIP(addr.IP, addr.Zone, allowed) {
			return nil, errUnsafeTarget
		}
	}
	var lastErr error
	for _, addr := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func parseAllowedNonPublicCIDRs(raw []string) []netip.Prefix {
	var allowed []netip.Prefix
	for _, value := range raw {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && !prefix.Addr().Is4In6() && prefix.Addr().Zone() == "" {
			allowed = append(allowed, prefix.Masked())
		}
	}
	return allowed
}

func isAllowedTargetIP(ip net.IP, zone string, allowed []netip.Prefix) bool {
	if zone != "" {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if isPublicIP(ip) {
		return true
	}
	for _, prefix := range allowed {
		if prefix.IsValid() && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() {
		return false
	}
	if addr.Is6() && !netip.MustParsePrefix("2000::/3").Contains(addr) {
		return false
	}
	for _, prefix := range nonPublicRanges {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func prefixes(raw ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}
