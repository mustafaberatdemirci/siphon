package run

import (
	"errors"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

func cfgOf(t *testing.T, cfgs []site.SiteConfig, name string) site.SiteConfig {
	t.Helper()
	for _, c := range cfgs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no site %s", name)
	return site.SiteConfig{}
}

// The window's or the command line's network goes to every site, its proxy
// only to the sites named; sites with the same route share a client, and
// a site with a DoH server can look names up the way its requests do.
func TestSetupGivesEachSiteItsRoute(t *testing.T) {
	cfgs, _, err := SetupWith(SetupOptions{Network: Network{
		DNS: "cloudflare", Proxy: "socks5://127.0.0.1:1080", ProxySites: []string{"bunkr", "cyberfile"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	bunkr, cyberfile, mega, gofile := cfgOf(t, cfgs, "bunkr"), cfgOf(t, cfgs, "cyberfile"), cfgOf(t, cfgs, "mega"), cfgOf(t, cfgs, "gofile")
	for _, c := range cfgs {
		if c.DNSResolver != "https://1.1.1.1/dns-query" || c.LookupHost == nil {
			t.Errorf("%s: DNS %q, lookup set: %v", c.Name, c.DNSResolver, c.LookupHost != nil)
		}
	}
	if bunkr.Proxy == "" || cyberfile.Proxy == "" || mega.Proxy != "" {
		t.Errorf("proxy: bunkr %q, cyberfile %q, mega %q", bunkr.Proxy, cyberfile.Proxy, mega.Proxy)
	}
	if bunkr.HTTPClient != cyberfile.HTTPClient {
		t.Error("two sites with the same route got two clients")
	}
	if bunkr.HTTPClient == mega.HTTPClient || mega.HTTPClient != gofile.HTTPClient {
		t.Error("the clients don't follow the routes")
	}
}

// Without a network of its own every site shares one client and the
// system's DNS, as before.
func TestSetupWithoutANetworkSharesOneClient(t *testing.T) {
	cfgs, _, err := SetupWith(SetupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfgs {
		if c.HTTPClient != cfgs[0].HTTPClient || c.LookupHost != nil || c.Proxy != "" || c.DNSResolver != "" {
			t.Errorf("%s has a route of its own", c.Name)
		}
	}
}

func TestSetupRefusesABadNetwork(t *testing.T) {
	for _, n := range []Network{{DNS: "8.8.8.8"}, {Proxy: "127.0.0.1:1080"}} {
		if _, _, err := SetupWith(SetupOptions{Network: n}); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: err = %v, want a usage error", n, err)
		}
	}
}
