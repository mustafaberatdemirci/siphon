package run

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
)

// After a network error the network is checked once per host, and a
// blocked site's line comes with its measurements.
func TestExplainNetwork(t *testing.T) {
	var asked []string
	check := func(_ context.Context, host string) netcheck.Finding {
		asked = append(asked, host)
		return netcheck.Finding{Host: host, DNSBlocked: host == "cyberfile.me", SystemIPs: []string{"195.175.254.2"}}
	}
	checked := map[string]bool{}
	timeout := &url.Error{Op: "Get", URL: "https://cyberfile.me/kwNO", Err: &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}}
	ctx := context.Background()

	note := explainNetwork(ctx, check, checked, timeout, "https://cyberfile.me/kwNO")
	if !strings.Contains(note, "your network's DNS blocks cyberfile.me") || !strings.Contains(note, "195.175.254.2") {
		t.Errorf("note %q", note)
	}
	if again := explainNetwork(ctx, check, checked, timeout, "https://cyberfile.me/other"); again != "" || len(asked) != 1 {
		t.Errorf("the same host was checked again (%v) or told again (%q)", asked, again)
	}
	if note := explainNetwork(ctx, check, checked, errors.New("HTTP 404"), "https://pixeldrain.com/u/x"); note != "" || len(asked) != 1 {
		t.Errorf("a site's own answer led to a network check: %q %v", note, asked)
	}
	if note := explainNetwork(ctx, nil, checked, timeout, ""); note != "" {
		t.Errorf("no checker, still a note: %q", note)
	}
}
