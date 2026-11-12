package doh

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeServer is a DoH server answering from a table: name -> addresses.
func fakeServer(t *testing.T, table map[string][]string, asked *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked != nil {
			asked.Add(1)
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil || r.Header.Get("Accept") != "application/dns-message" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(raw)
		if err != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		q, err := p.Question()
		if err != nil {
			http.Error(w, "no question", http.StatusBadRequest)
			return
		}
		name := q.Name.String()
		name = name[:len(name)-1]
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 300}
		for _, s := range table[name] {
			ip := netip.MustParseAddr(s)
			switch {
			case ip.Is4() && q.Type == dnsmessage.TypeA:
				_ = b.AResource(rh, dnsmessage.AResource{A: ip.As4()})
			case ip.Is6() && q.Type == dnsmessage.TypeAAAA:
				_ = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: ip.As16()})
			}
		}
		msg, _ := b.Finish()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(msg)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLookupHostAsksAndCaches(t *testing.T) {
	var asked atomic.Int32
	srv := fakeServer(t, map[string][]string{"files.example": {"203.0.113.7", "2001:db8::7"}}, &asked)
	r := New(srv.URL+"/dns-query", srv.Client())

	ips, err := r.LookupHost(context.Background(), "Files.Example.")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 2 || ips[0] != "203.0.113.7" || ips[1] != "2001:db8::7" {
		t.Fatalf("ips = %v, want the IPv4 address, then the IPv6 one", ips)
	}
	n := asked.Load()
	if _, err := r.LookupHost(context.Background(), "files.example"); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != n {
		t.Error("a cached name was asked again")
	}
	// After the TTL it is asked again.
	r.now = func() time.Time { return time.Now().Add(maxTTL + time.Second) }
	if _, err := r.LookupHost(context.Background(), "files.example"); err != nil {
		t.Fatal(err)
	}
	if asked.Load() == n {
		t.Error("an expired name wasn't asked again")
	}
}

// A failure is a *net.DNSError: the rest of Siphon (retries, bunkr's domain
// rotation, the network check) treats it as it treats the system's.
func TestFailuresAreDNSErrors(t *testing.T) {
	srv := fakeServer(t, map[string][]string{}, nil)
	r := New(srv.URL+"/dns-query", srv.Client())
	_, err := r.LookupHost(context.Background(), "nowhere.example")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("an unknown name gave %v, want a not-found DNS error", err)
	}

	down := New("https://127.0.0.1:1/dns-query", &http.Client{Timeout: time.Second})
	_, err = down.LookupHost(context.Background(), "files.example")
	if !errors.As(err, &dnsErr) || !dnsErr.IsTemporary {
		t.Errorf("an unreachable resolver gave %v, want a temporary DNS error", err)
	}
}

func TestAnAddressNeedsNoQuestion(t *testing.T) {
	r := New("https://127.0.0.1:1/dns-query", nil)
	for _, in := range []string{"192.0.2.1", "2001:db8::1"} {
		ips, err := r.LookupHost(context.Background(), in)
		if err != nil || len(ips) != 1 || ips[0] != in {
			t.Errorf("LookupHost(%s) = %v, %v", in, ips, err)
		}
	}
}

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		"system":                            "",
		"Cloudflare":                        "https://1.1.1.1/dns-query",
		"google":                            "https://8.8.8.8/dns-query",
		"quad9":                             "https://9.9.9.9/dns-query",
		"https://dns.example/dns-query?x=1": "https://dns.example/dns-query?x=1",
	} {
		got, err := Endpoint(in)
		if err != nil || got != want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"1.1.1.1", "http://1.1.1.1/dns-query", "tls://dns.example"} {
		if _, err := Endpoint(bad); err == nil {
			t.Errorf("Endpoint(%q) was accepted", bad)
		}
	}
}

// The dialer connects to the address the resolver gave, whatever the
// system's DNS says about the name.
func TestDialerConnectsToTheResolvedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	srv := fakeServer(t, map[string][]string{"blocked.example": {"127.0.0.1"}}, nil)
	d := Dialer{Resolver: New(srv.URL+"/dns-query", srv.Client()), Dialer: &net.Dialer{Timeout: 2 * time.Second}}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("blocked.example", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Against the real resolvers; skipped unless SIPHON_LIVE=1.
func TestLiveResolvers(t *testing.T) {
	if os.Getenv("SIPHON_LIVE") != "1" {
		t.Skip("set SIPHON_LIVE=1 to ask the real resolvers")
	}
	for _, name := range Names() {
		u, _ := Endpoint(name)
		ips, err := New(u, nil).LookupHost(context.Background(), "example.com")
		if err != nil || len(ips) == 0 {
			t.Errorf("%s: %v %v", name, ips, err)
			continue
		}
		t.Logf("%s: example.com -> %v", name, ips)
	}
}
