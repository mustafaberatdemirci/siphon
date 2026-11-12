// Package doh resolves names over DNS-over-HTTPS (RFC 8484).
//
// Why Siphon asks a DNS server of its own: MEASURED 2026-10-19 on a Turkish
// home connection, the provider's DNS answered bunkr.cr and cyberfile.me
// with its block page (195.175.254.2) while a public resolver gave the
// sites' real addresses. Siphon could tell the user so (internal/netcheck)
// but not get around it; asked over HTTPS, at the resolver's IP address, the
// provider's DNS has no say. A network that reads the site's name in the
// TLS handshake and drops the connection is not got around this way: that
// takes a VPN or a proxy.
package doh

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Known resolvers, at their IP addresses so that asking them needs no DNS.
// Each one's certificate names the address.
var known = map[string]string{
	"cloudflare": "https://1.1.1.1/dns-query",
	"google":     "https://8.8.8.8/dns-query",
	"quad9":      "https://9.9.9.9/dns-query",
}

// Names lists the known resolvers' names, in the order a window offers them.
func Names() []string { return []string{"cloudflare", "google", "quad9"} }

// Endpoint turns a setting into a DoH address: "" or "system" is the
// system's DNS (""), a known name its address, an https URL itself.
func Endpoint(setting string) (string, error) {
	s := strings.TrimSpace(setting)
	if s == "" || strings.EqualFold(s, "system") {
		return "", nil
	}
	if u, ok := known[strings.ToLower(s)]; ok {
		return u, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("DNS %q: give system, cloudflare, google, quad9 or an https:// DoH address", setting)
	}
	return s, nil
}

// Cache bounds: an answer is kept for its TTL, within these.
const (
	minTTL = 30 * time.Second
	maxTTL = 10 * time.Minute
)

// Resolver asks one DoH server. Safe for concurrent use.
type Resolver struct {
	endpoint string
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ips   []string
	until time.Time
}

// New returns a resolver asking endpoint (see Endpoint). client may be nil.
func New(endpoint string, client *http.Client) *Resolver {
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			// The resolver is reached directly: a proxy meant for a site must
			// not carry its DNS questions, and the environment's isn't Siphon's.
			Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 8 * time.Second},
		}
	}
	return &Resolver{endpoint: endpoint, client: client, now: time.Now, cache: map[string]cached{}}
}

// Endpoint is the address the resolver asks.
func (r *Resolver) Endpoint() string { return r.endpoint }

// LookupHost returns host's addresses: IPv4 first, then IPv6. An address
// is returned as it is. A failure is a *net.DNSError, so the rest of
// Siphon tells it from a refused connection the way it does a system DNS
// failure.
func (r *Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return []string{ip.String()}, nil
	}
	r.mu.Lock()
	if c, ok := r.cache[host]; ok && r.now().Before(c.until) {
		r.mu.Unlock()
		return c.ips, nil
	}
	r.mu.Unlock()

	var ips []string
	ttl := maxTTL
	var errs []error
	for _, t := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		got, gotTTL, err := r.ask(ctx, host, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ips = append(ips, got...)
		if len(got) > 0 && gotTTL < ttl {
			ttl = gotTTL
		}
	}
	if len(ips) == 0 {
		dnsErr := &net.DNSError{Name: host, Server: r.endpoint, Err: "no address", IsNotFound: true}
		if err := errors.Join(errs...); err != nil {
			dnsErr.Err, dnsErr.IsNotFound, dnsErr.IsTemporary = err.Error(), false, true
		}
		return nil, dnsErr
	}
	ttl = max(minTTL, min(ttl, maxTTL))
	r.mu.Lock()
	r.cache[host] = cached{ips: ips, until: r.now().Add(ttl)}
	r.mu.Unlock()
	return ips, nil
}

// ask sends one question and returns the addresses of type t in the
// answer and their smallest TTL.
func (r *Resolver) ask(ctx context.Context, host string, t dnsmessage.Type) ([]string, time.Duration, error) {
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, 0, fmt.Errorf("%s is not a name: %w", host, err)
	}
	// ID 0, as RFC 8484 recommends: the answer is cacheable by the HTTP layer.
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, 0, err
	}
	if err := b.Question(dnsmessage.Question{Name: name, Type: t, Class: dnsmessage.ClassINET}); err != nil {
		return nil, 0, err
	}
	q, err := b.Finish()
	if err != nil {
		return nil, 0, err
	}

	u := r.endpoint
	if strings.Contains(u, "?") {
		u += "&"
	} else {
		u += "?"
	}
	u += "dns=" + base64.RawURLEncoding.EncodeToString(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-message")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s: HTTP %s", r.endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, 0, err
	}
	return parse(body, t)
}

// parse reads the addresses of type t from a DNS answer. CNAMEs on the way
// are left out: a resolver answers with the chain and its end.
func parse(msg []byte, t dnsmessage.Type) ([]string, time.Duration, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, 0, fmt.Errorf("unreadable DNS answer: %w", err)
	}
	if h.RCode != dnsmessage.RCodeSuccess && h.RCode != dnsmessage.RCodeNameError {
		return nil, 0, fmt.Errorf("DNS answer: %v", h.RCode)
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, err
	}
	var ips []string
	ttl := maxTTL
	for {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if rh.Type != t || rh.Class != dnsmessage.ClassINET {
			if err := p.SkipAnswer(); err != nil {
				return nil, 0, err
			}
			continue
		}
		switch t {
		case dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				return nil, 0, err
			}
			ips = append(ips, netip.AddrFrom4(a.A).String())
		case dnsmessage.TypeAAAA:
			a, err := p.AAAAResource()
			if err != nil {
				return nil, 0, err
			}
			ips = append(ips, netip.AddrFrom16(a.AAAA).String())
		}
		if d := time.Duration(rh.TTL) * time.Second; d < ttl {
			ttl = d
		}
	}
	return ips, ttl, nil
}

// Dialer dials a host's address found with r, trying them in order.
type Dialer struct {
	Resolver *Resolver
	Dialer   *net.Dialer
}

// DialContext is for http.Transport.DialContext.
func (d Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := d.Resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, ip := range ips {
		c, err := d.Dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	// The first failure says the most (the preferred address's); it is a
	// *net.OpError like any refused connection.
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return nil, &net.DNSError{Name: host, Server: d.Resolver.endpoint, Err: "no address", IsNotFound: true}
}
