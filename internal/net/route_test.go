package net

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mustafaberatdemirci/siphon/internal/doh"
)

func TestParseProxy(t *testing.T) {
	for _, ok := range []string{"socks5://127.0.0.1:1080", "socks5h://proxy.example:9050", "http://user:pass@10.0.0.1:3128", "https://proxy.example:443"} {
		if _, err := ParseProxy(ok); err != nil {
			t.Errorf("ParseProxy(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:1080", "socks4://127.0.0.1:1080", "socks5://127.0.0.1", "ftp://x:21", "http://:8080"} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("ParseProxy(%q) was accepted", bad)
		}
	}
}

// A site's requests go through its proxy: an http:// request reaches the
// proxy with the whole URL, a name no DNS knows included.
func TestClientForGoesThroughTheProxy(t *testing.T) {
	var through atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "files.example" {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		through.Add(1)
		_, _ = io.WriteString(w, "via the proxy")
	}))
	defer proxy.Close()

	c, err := NewClientFor(proxy.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://files.example/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via the proxy" || through.Load() != 1 {
		t.Errorf("got %q, %d requests through the proxy", body, through.Load())
	}
}

// A site's names are resolved by its DoH resolver: a name the system's DNS
// doesn't know reaches the address the resolver gives.
func TestClientForResolvesWithDoH(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the real server")
	}))
	defer target.Close()
	resolver := fakeDoH(t, "blocked.example", "127.0.0.1")

	c, err := NewClientFor("", resolver)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	resp, err := c.Get("http://blocked.example:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "the real server" {
		t.Errorf("got %q", body)
	}

	// A name the resolver doesn't know is a DNS error, as with the system's.
	_, err = c.Get("http://nowhere.example:" + port + "/")
	var dnsErr *net.DNSError
	if !IsTransient(err) || !errors.As(err, &dnsErr) {
		t.Errorf("an unknown name gave %v, want a DNS error", err)
	}
}

// fakeDoH is a DoH server that knows one name, and a resolver asking it.
func fakeDoH(t *testing.T, name, ip string) *doh.Resolver {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		var p dnsmessage.Parser
		if _, err := p.Start(raw); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		q, _ := p.Question()
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		if q.Name.String() == name+"." && q.Type == dnsmessage.TypeA {
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60},
				dnsmessage.AResource{A: netip.MustParseAddr(ip).As4()})
		}
		msg, _ := b.Finish()
		_, _ = w.Write(msg)
	}))
	t.Cleanup(srv.Close)
	return doh.New(srv.URL+"/dns-query", srv.Client())
}
