package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// A proxy no site is ticked for is no proxy: the engine would take an
// empty list of sites as every site.
func TestNetworkFromPrefs(t *testing.T) {
	a := test.NewTempApp(t)
	p := a.Preferences()
	p.SetString(prefDNS, "quad9")
	p.SetString(prefProxy, "socks5://127.0.0.1:1080")

	n := networkFromPrefs(p)
	if n.DNS != "quad9" || n.Proxy != "" || len(n.ProxySites) != 0 {
		t.Errorf("no site ticked: %+v, want the DNS and no proxy", n)
	}
	p.SetString(prefProxySites, " bunkr, ,cyberfile ")
	n = networkFromPrefs(p)
	if n.Proxy != "socks5://127.0.0.1:1080" || len(n.ProxySites) != 2 || n.ProxySites[0] != "bunkr" || n.ProxySites[1] != "cyberfile" {
		t.Errorf("two sites ticked: %+v", n)
	}
}
