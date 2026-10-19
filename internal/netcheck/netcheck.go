// Package netcheck tells whether the user's network blocks a site, and how.
//
// Measured (2026-10-19, a Turkish home connection): bunkr.cr and cyberfile.me
// resolved to 195.175.254.2, the provider's block page, while public DNS gave
// their real addresses; and a TLS handshake naming bunkr.cr, cyberfile.me or
// www.mediafire.com got no answer even at the real address, while the same
// servers answered at once under another name. Siphon only said "timeout" or
// "connection reset", and the user looked for the fault in Siphon.
//
// Two checks, both only reading:
//
//   - DNS: the system's answer is compared with a public resolver's (asked
//     over HTTPS at its IP address, so the local DNS has no say). Different
//     addresses alone prove nothing, CDNs answer by region; they count when
//     the system's address doesn't serve the site (no handshake, or a
//     certificate that isn't the site's).
//   - The site's name: a handshake naming the site that gets no answer, while
//     the same address answers a handshake under another name, means
//     something on the way reads the name and drops the connection.
//
// The first is solved by another DNS server; the second only by a VPN (or a
// network that doesn't filter).
package netcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Finding is what the checks found about a host.
type Finding struct {
	Host string
	// SystemIPs are the system resolver's IPv4 answers; SystemErr why there
	// were none.
	SystemIPs []string
	SystemErr error
	// PublicIPs are the public resolver's; empty when it couldn't be asked.
	PublicIPs []string
	Public    string // which resolver answered ("1.1.1.1")

	// DNSBlocked: the system's DNS gives no address, or addresses that don't
	// serve the site, while the public resolver's do.
	DNSBlocked bool
	// NameFiltered: a handshake naming the site gets no answer where the same
	// address answers under another name.
	NameFiltered bool
	// Unreachable: the site's address doesn't answer at all.
	Unreachable bool
	// Reached: a handshake naming the site succeeded with its certificate.
	Reached bool
	// Address is the address the handshakes were made with.
	Address string
}

// Blocked reports whether the network blocks the site.
func (f Finding) Blocked() bool { return f.DNSBlocked || f.NameFiltered }

// Summary is one line for the user.
func (f Finding) Summary() string {
	switch {
	case f.DNSBlocked && f.NameFiltered:
		return fmt.Sprintf("your network blocks %s: its DNS gives a wrong address, and connections naming the site are cut; a VPN gets around it, another DNS server alone doesn't", f.Host)
	case f.NameFiltered:
		return fmt.Sprintf("your network blocks %s: connections naming the site are cut, while the same server answers under another name; a VPN gets around it", f.Host)
	case f.DNSBlocked:
		return fmt.Sprintf("your network's DNS blocks %s: it gives an address that doesn't serve the site; another DNS server or a VPN gets around it", f.Host)
	case f.Unreachable:
		return fmt.Sprintf("%s doesn't answer at %s (the server is down, or your network blocks its address)", f.Host, f.Address)
	case f.Reached:
		return fmt.Sprintf("your network reaches %s normally", f.Host)
	}
	return fmt.Sprintf("could not tell whether your network blocks %s", f.Host)
}

// Evidence is the measurements behind the summary, for doctor's detail line.
func (f Finding) Evidence() string {
	var parts []string
	if f.SystemErr != nil {
		parts = append(parts, "system DNS: "+firstLine(f.SystemErr.Error()))
	} else {
		parts = append(parts, "system DNS: "+strings.Join(f.SystemIPs, ", "))
	}
	if f.Public != "" {
		parts = append(parts, fmt.Sprintf("public DNS (%s): %s", f.Public, strings.Join(f.PublicIPs, ", ")))
	}
	return strings.Join(parts, "; ")
}

// Handshake is the result of one TLS handshake.
type Handshake int

const (
	// Silent: no answer (timeout, connection reset or closed): what a filter
	// that drops the connection looks like.
	Silent Handshake = iota
	// Answered: the server spoke TLS, but its certificate isn't the name's
	// (or it refused the name with an alert).
	Answered
	// Verified: the handshake succeeded with a certificate for the name.
	Verified
)

// Checker makes the checks. Its fields are the network operations, so tests
// can play any network; a zero Checker uses the real ones.
type Checker struct {
	LookupSystem func(ctx context.Context, host string) ([]string, error)
	LookupPublic func(ctx context.Context, host string) (ips []string, resolver string, err error)
	Handshake    func(ctx context.Context, ip, name string) Handshake
}

// controlName is the name the control handshake uses: any name that is not
// the site's. A server answers a name it doesn't host with its default
// certificate or an alert, both an answer; a filter has no reason to cut it.
const controlName = "example.com"

// Check runs the checks for host.
func Check(ctx context.Context, host string) Finding { return Checker{}.Check(ctx, host) }

// Check runs the checks for host.
func (c Checker) Check(ctx context.Context, host string) Finding {
	if c.LookupSystem == nil {
		c.LookupSystem = lookupSystem
	}
	if c.LookupPublic == nil {
		c.LookupPublic = LookupPublic
	}
	if c.Handshake == nil {
		c.Handshake = TLSHandshake
	}
	f := Finding{Host: host}
	f.SystemIPs, f.SystemErr = c.LookupSystem(ctx, host)
	if pub, which, err := c.LookupPublic(ctx, host); err == nil && len(pub) > 0 {
		f.PublicIPs, f.Public = pub, which
	}

	// The address the site really is at: the system's when the public
	// resolver agrees or can't be asked, the public one's otherwise.
	var real string
	switch {
	case len(f.PublicIPs) > 0 && (len(f.SystemIPs) == 0 || !overlap(f.SystemIPs, f.PublicIPs)):
		real = f.PublicIPs[0]
		if len(f.SystemIPs) == 0 {
			f.DNSBlocked = true // the system's DNS hides the site
		} else if c.Handshake(ctx, f.SystemIPs[0], host) != Verified {
			// Regional CDN answers serve the site; a block page doesn't.
			f.DNSBlocked = true
		}
	case len(f.SystemIPs) > 0:
		real = f.SystemIPs[0]
	default:
		return f // nothing to connect to
	}
	f.Address = real

	switch c.Handshake(ctx, real, host) {
	case Verified:
		f.Reached = !f.DNSBlocked
	case Answered:
		// The server at the site's address speaks but isn't the site: an
		// interception (or the public answer is stale). Nothing to conclude.
	case Silent:
		if c.Handshake(ctx, real, controlName) == Silent {
			f.Unreachable = true
		} else {
			f.NameFiltered = true
		}
	}
	return f
}

func overlap(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// lookupSystem is the system resolver's IPv4 answers (the public resolver
// is asked for IPv4 only; comparing IPv6 with nothing would be noise).
func lookupSystem(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no IPv4 address", host)
	}
	return out, nil
}

// publicResolvers are DNS-over-HTTPS endpoints at IP addresses, so asking
// them needs no DNS; both answer the same JSON form.
var publicResolvers = []struct{ name, url string }{
	{"1.1.1.1", "https://1.1.1.1/dns-query"},
	{"8.8.8.8", "https://8.8.8.8/resolve"},
}

// LookupPublic asks the public resolvers in turn for host's IPv4 addresses.
func LookupPublic(ctx context.Context, host string) ([]string, string, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	var errs []error
	for _, r := range publicResolvers {
		ips, err := askDoH(ctx, client, r.url, host)
		if err == nil {
			return ips, r.name, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", r.name, err))
	}
	return nil, "", errors.Join(errs...)
}

func askDoH(ctx context.Context, client *http.Client, endpoint, host string) ([]string, error) {
	q := url.Values{"name": {host}, "type": {"A"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return parseDoH(resp.Body)
}

// parseDoH reads the JSON DNS answer: the A records, CNAMEs on the way left out.
func parseDoH(r io.Reader) ([]string, error) {
	var ans struct {
		Status int
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		}
	}
	if err := json.NewDecoder(r).Decode(&ans); err != nil {
		return nil, err
	}
	if ans.Status != 0 {
		return nil, fmt.Errorf("DNS status %d", ans.Status)
	}
	var out []string
	for _, a := range ans.Answer {
		if ip := net.ParseIP(a.Data); a.Type == 1 && ip != nil && ip.To4() != nil {
			out = append(out, ip.String())
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no A record")
	}
	return out, nil
}

// handshakeTimeout: a filter that drops the connection shows as silence, so
// waiting longer only makes the report slower.
var handshakeTimeout = 6 * time.Second

// TLSHandshake connects to ip:443 naming name and tells how it went.
func TLSHandshake(ctx context.Context, ip, name string) Handshake {
	return handshakeAt(ctx, net.JoinHostPort(ip, "443"), name, nil)
}

// handshakeAt is TLSHandshake at any address; roots nil means the system's.
func handshakeAt(ctx context.Context, addr, name string, roots *x509.CertPool) Handshake {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return Silent
	}
	defer raw.Close()
	cfg := &tls.Config{ServerName: name, InsecureSkipVerify: true}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		var alert tls.AlertError
		if errors.As(err, &alert) {
			return Answered // a refusal is an answer
		}
		return Silent
	}
	// Verified by hand: InsecureSkipVerify above, so a wrong certificate
	// still tells "it answered".
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Answered
	}
	opts := x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range certs[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		return Answered
	}
	return Verified
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// HostOfError is the host a request's error happened on: the URL in err,
// else fallbackURL's host (a web link only); "" when neither names one.
func HostOfError(err error, fallbackURL string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if u, perr := url.Parse(fallbackURL); perr == nil && u.Hostname() != "" &&
		(u.Scheme == "http" || u.Scheme == "https") {
		return u.Hostname()
	}
	return ""
}
