package main

// Settings > DNS and Proxy: how the sites' requests leave (run.Network).
// The engine builds each site's connection at start, so a change takes
// effect after a restart, which Siphon offers when Settings closes.

import (
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/doh"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

const (
	prefDNS        = "dns"         // "", or a name doh.Endpoint knows
	prefProxy      = "proxy"       // a proxy URL, "" for none
	prefProxySites = "proxy_sites" // comma-separated site names
)

// networkFromPrefs is the network the engine is built with. A proxy no
// site is ticked for is no proxy (run.Network would take no sites as all).
func networkFromPrefs(p fyne.Preferences) run.Network {
	n := run.Network{DNS: p.String(prefDNS)}
	if proxy := strings.TrimSpace(p.String(prefProxy)); proxy != "" {
		if sites := splitSites(p.String(prefProxySites)); len(sites) > 0 {
			n.Proxy, n.ProxySites = proxy, sites
		}
	}
	return n
}

func splitSites(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// networkSnapshot is what a change of needs a restart.
func networkSnapshot(p fyne.Preferences) string {
	return p.String(prefDNS) + "|" + p.String(prefProxy) + "|" + p.String(prefProxySites)
}

type dnsChoice struct{ code, name string }

// dnsChoices are the DNS setting's entries: the system's, then the known
// resolvers.
func dnsChoices() []dnsChoice {
	labels := map[string]string{"cloudflare": "Cloudflare (1.1.1.1)", "google": "Google (8.8.8.8)", "quad9": "Quad9 (9.9.9.9)"}
	out := []dnsChoice{{"", T("System")}}
	for _, n := range doh.Names() {
		out = append(out, dnsChoice{n, labels[n]})
	}
	return out
}

// dnsSetting is the DNS setting's select.
func (q *queueTab) dnsSetting() fyne.CanvasObject {
	choices := dnsChoices()
	names := make([]string, len(choices))
	for i, c := range choices {
		names[i] = c.name
	}
	sel := widget.NewSelect(names, nil)
	saved := strings.ToLower(q.prefs.String(prefDNS))
	sel.SetSelected(names[0])
	for _, c := range choices {
		if c.code == saved {
			sel.SetSelected(c.name)
		}
	}
	sel.OnChanged = func(name string) {
		for _, c := range choices {
			if c.name == name {
				q.prefs.SetString(prefDNS, c.code)
			}
		}
	}
	return sel
}

// siteLabel is how a site is named in the window.
func siteLabel(name string) string {
	switch name {
	case site.DirectName:
		return T("Direct links")
	case site.GdriveName:
		return "Google Drive"
	case site.DropboxName:
		return "Dropbox"
	}
	return name
}

// proxySetting is the proxy's address and the sites it is for.
func (q *queueTab) proxySetting() fyne.CanvasObject {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("socks5://127.0.0.1:1080")
	entry.SetText(q.prefs.String(prefProxy))
	entry.Validator = func(s string) error {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		_, err := snet.ParseProxy(s)
		return err
	}
	entry.OnChanged = func(s string) {
		if entry.Validator(s) == nil {
			q.prefs.SetString(prefProxy, strings.TrimSpace(s))
		}
	}

	ticked := map[string]bool{}
	for _, s := range splitSites(q.prefs.String(prefProxySites)) {
		ticked[s] = true
	}
	names := q.eng.Sites()
	sort.SliceStable(names, func(i, j int) bool { // direct links last
		return names[i] != site.DirectName && names[j] == site.DirectName
	})
	save := func() {
		var on []string
		for _, n := range names {
			if ticked[n] {
				on = append(on, n)
			}
		}
		q.prefs.SetString(prefProxySites, strings.Join(on, ","))
	}
	checks := make([]fyne.CanvasObject, 0, len(names))
	for _, n := range names {
		c := widget.NewCheck(siteLabel(n), func(b bool) {
			ticked[n] = b
			save()
		})
		c.SetChecked(ticked[n])
		checks = append(checks, c)
	}
	return container.NewVBox(entry, container.NewGridWithColumns(4, checks...))
}
