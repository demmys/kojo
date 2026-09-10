package customapi

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// LocalRelay pins an operator-configured custom API endpoint to the addresses
// validated at startup and exposes it only on a random loopback port. CLI
// clients such as Claude Code can then use the relay without getting a second
// chance to resolve the hostname, follow an upstream redirect, or honor a
// process-wide outbound proxy for the credential-bearing request.
type LocalRelay struct {
	URL       string
	server    *http.Server
	done      chan struct{}
	closeIdle func()
	closeOnce sync.Once
}

// StartLocalRelay starts a reverse proxy from loopback to baseURL. The relay
// injects the upstream Bearer token itself so it never has to enter the CLI
// process environment, and rejects upstream redirects so the credential
// cannot be forwarded to another origin by the CLI client.
func StartLocalRelay(ctx context.Context, baseURL, apiKey string) (*LocalRelay, error) {
	target, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	client, err := NewLocalOrTailnetClient(ctx, baseURL, 0)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for custom API relay: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		// Never trust an Authorization header supplied by the CLI. The
		// operator-owned credential remains inside kojo and is attached only
		// after the target has been validated and DNS-pinned.
		req.Header.Del("X-Api-Key")
		if apiKey == "" {
			req.Header.Del("Authorization")
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	proxy.Transport = client.Transport
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("custom API redirect refused: HTTP %d", resp.StatusCode)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, proxyErr error) {
		http.Error(w, proxyErr.Error(), http.StatusBadGateway)
	}

	relay := &LocalRelay{
		URL:       "http://" + listener.Addr().String(),
		done:      make(chan struct{}),
		closeIdle: client.CloseIdleConnections,
	}
	relay.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = relay.server.Serve(listener)
		close(relay.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			relay.Close()
		case <-relay.done:
		}
	}()
	return relay, nil
}

// Close stops the relay and all active connections.
func (r *LocalRelay) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		_ = r.server.Close()
		r.closeIdle()
	})
}

// NewLocalOrTailnetClient validates baseURL and returns an HTTP client whose
// dialer is pinned to the loopback or Tailscale addresses resolved during
// validation. Pinning matters for hostname inputs: validating one DNS answer
// and then letting http.Transport resolve the name again would leave a DNS
// rebinding window into arbitrary private-network services.
func NewLocalOrTailnetClient(ctx context.Context, baseURL string, timeout time.Duration) (*http.Client, error) {
	u, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	host := u.Hostname()
	addrs, err := resolveAllowed(ctx, host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		dialHost, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, fmt.Errorf("split dial address: %w", splitErr)
		}
		if !strings.EqualFold(strings.TrimSuffix(dialHost, "."), strings.TrimSuffix(host, ".")) {
			return nil, fmt.Errorf("custom API redirect host %q is not allowed", dialHost)
		}
		var lastErr error
		for _, ip := range addrs {
			conn, dialErr := dialer.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("dial custom API: %w", lastErr)
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// API endpoints are not expected to redirect. Refusing redirects
			// also keeps credentials from crossing to another origin.
			return http.ErrUseLastResponse
		},
	}, nil
}

// ParseBaseURL performs the non-network portion of custom API URL validation.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("URL host is required")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URL userinfo is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("URL query and fragment are not allowed")
	}
	return u, nil
}

func resolveAllowed(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !isAllowedIP(ip) {
			return nil, fmt.Errorf("only loopback or Tailscale addresses are allowed, got %q", host)
		}
		return []net.IP{ip}, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resolved, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve custom API host %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("custom API host %q resolved to no addresses", host)
	}

	addrs := make([]net.IP, 0, len(resolved))
	for _, candidate := range resolved {
		if !isAllowedIP(candidate.IP) {
			return nil, fmt.Errorf("custom API host %q resolved outside loopback/Tailscale: %s", host, candidate.IP)
		}
		addrs = append(addrs, candidate.IP)
	}
	return addrs, nil
}

func isAllowedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)
}
