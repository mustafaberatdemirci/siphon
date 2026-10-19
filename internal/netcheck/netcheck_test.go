package netcheck

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// network plays a network: DNS answers and how each (address, name)
// handshake goes; anything not listed is silent.
type network struct {
	system    []string
	systemErr error
	public    []string
	hs        map[string]Handshake // "ip name"
}

func (n network) checker() Checker {
	return Checker{
		LookupSystem: func(context.Context, string) ([]string, error) { return n.system, n.systemErr },
		LookupPublic: func(context.Context, string) ([]string, string, error) {
			if n.public == nil {
				return nil, "", errors.New("unreachable")
			}
			return n.public, "1.1.1.1", nil
		},
		Handshake: func(_ context.Context, ip, name string) Handshake { return n.hs[ip+" "+name] },
	}
}

func TestCheck(t *testing.T) {
	const site = "files.example"
	for _, tc := range []struct {
		name string
		net  network
		want Finding // only the verdict fields are compared
	}{
		{"reached", network{system: []string{"1.2.3.4"}, public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"1.2.3.4 " + site: Verified}},
			Finding{Reached: true}},
		// What the measurement showed for bunkr.cr and cyberfile.me.
		{"block page and name filter", network{system: []string{"9.9.9.9"}, public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"9.9.9.9 " + site: Answered, "1.2.3.4 " + controlName: Answered}},
			Finding{DNSBlocked: true, NameFiltered: true}},
		// What it showed for www.mediafire.com.
		{"name filter only", network{system: []string{"1.2.3.4"}, public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"1.2.3.4 " + controlName: Answered}},
			Finding{NameFiltered: true}},
		{"DNS block only", network{system: []string{"9.9.9.9"}, public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"1.2.3.4 " + site: Verified}},
			Finding{DNSBlocked: true}},
		{"DNS hides the site", network{systemErr: errors.New("no such host"), public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"1.2.3.4 " + site: Verified}},
			Finding{DNSBlocked: true}},
		// A CDN answering by region: other addresses, but they serve the site.
		{"regional answer", network{system: []string{"5.5.5.5"}, public: []string{"1.2.3.4"},
			hs: map[string]Handshake{"5.5.5.5 " + site: Verified, "1.2.3.4 " + site: Verified}},
			Finding{Reached: true}},
		{"server down", network{system: []string{"1.2.3.4"}, public: []string{"1.2.3.4"}},
			Finding{Unreachable: true}},
		// No public resolver to ask: the name check still runs on the system's address.
		{"no public DNS", network{system: []string{"1.2.3.4"},
			hs: map[string]Handshake{"1.2.3.4 " + controlName: Answered}},
			Finding{NameFiltered: true}},
		{"nothing resolves", network{systemErr: errors.New("no such host")}, Finding{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.net.checker().Check(context.Background(), site)
			verdict := func(f Finding) [4]bool { return [4]bool{f.DNSBlocked, f.NameFiltered, f.Unreachable, f.Reached} }
			if verdict(f) != verdict(tc.want) {
				t.Errorf("DNSBlocked, NameFiltered, Unreachable, Reached = %v, want %v", verdict(f), verdict(tc.want))
			}
			if s := f.Summary(); !strings.Contains(s, site) {
				t.Errorf("summary %q doesn't name the site", s)
			}
		})
	}
}

func TestSummarySaysWhatHelps(t *testing.T) {
	both := Finding{Host: "h", DNSBlocked: true, NameFiltered: true}.Summary()
	if !strings.Contains(both, "VPN") || !strings.Contains(both, "another DNS server alone doesn't") {
		t.Errorf("both blocks: %q", both)
	}
	if s := (Finding{Host: "h", DNSBlocked: true}).Summary(); !strings.Contains(s, "another DNS server") {
		t.Errorf("DNS block: %q", s)
	}
	if !(Finding{NameFiltered: true}).Blocked() || (Finding{Unreachable: true}).Blocked() {
		t.Error("Blocked() is wrong")
	}
}

func TestHandshake(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	addr := srv.Listener.Addr().String()
	ctx := context.Background()

	if got := handshakeAt(ctx, addr, "example.com", roots); got != Verified {
		t.Errorf("the server's own name: %v, want Verified", got)
	}
	if got := handshakeAt(ctx, addr, "other.test", roots); got != Answered {
		t.Errorf("a name the certificate doesn't cover: %v, want Answered", got)
	}

	// A filter: takes the connection and never answers, or closes it.
	for _, behavior := range []string{"hang", "close"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				if behavior == "close" {
					c.Close()
				} else {
					defer c.Close()
				}
			}
		}()
		if got := handshakeAt(ctx, ln.Addr().String(), "example.com", roots); got != Silent {
			t.Errorf("%s: %v, want Silent", behavior, got)
		}
		ln.Close()
	}
}

func TestParseDoH(t *testing.T) {
	// Cloudflare's form, a CNAME on the way.
	ips, err := parseDoH(strings.NewReader(`{"Status":0,"Answer":[
		{"name":"www.x","type":5,"data":"x.cdn."},
		{"name":"x.cdn","type":1,"data":"104.18.214.225"},
		{"name":"x.cdn","type":1,"data":"104.18.213.225"}]}`))
	if err != nil || strings.Join(ips, ",") != "104.18.214.225,104.18.213.225" {
		t.Errorf("got %v %v", ips, err)
	}
	if _, err := parseDoH(strings.NewReader(`{"Status":3}`)); err == nil {
		t.Error("NXDOMAIN gave no error")
	}
	if _, err := parseDoH(strings.NewReader(`{"Status":0,"Answer":[]}`)); err == nil {
		t.Error("an empty answer gave no error")
	}
}

func TestAskDoHSendsTheQuestion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "files.example" || r.URL.Query().Get("type") != "A" ||
			r.Header.Get("Accept") != "application/dns-json" {
			http.Error(w, "bad question", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"Status":0,"Answer":[{"type":1,"data":"1.2.3.4"}]}`))
	}))
	t.Cleanup(srv.Close)
	ips, err := askDoH(context.Background(), srv.Client(), srv.URL+"/dns-query", "files.example")
	if err != nil || len(ips) != 1 || ips[0] != "1.2.3.4" {
		t.Errorf("got %v %v", ips, err)
	}
}
