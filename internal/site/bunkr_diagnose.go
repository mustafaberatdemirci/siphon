package site

// bunkr: doctor's layer diagnosis.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func (b *bunkr) Diagnose(ctx context.Context) ([]LayerResult, error) {
	if len(b.cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL list is empty")
	}
	var last []LayerResult
	for _, canary := range b.cfg.CanaryURLs {
		res := b.diagnoseOne(ctx, canary)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (b *bunkr) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	out := make([]LayerResult, 0, 6)

	u, err := url.Parse(canary)
	if err != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "could not parse the canary URL", Evidence: canary})
	}
	host := normalizeHost(u.Host)

	addrs, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if dnsErr != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "did not resolve", Evidence: host + ": " + dnsErr.Error()})
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out = append(out, LayerResult{Layer: LayerDNS, Status: StatusOK,
		Detail: fmt.Sprintf("%d addresses", len(ips)), Evidence: strings.Join(ips, ", ")})

	body, err := b.get(ctx, canary, "")
	// The response is recorded in every case: the broken page's body is the
	// real evidence, but a working body is the reference for a future diff.
	b.cfg.Recordln("canary.html", body)

	switch {
	case err == nil:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"},
			LayerResult{Layer: LayerFetch, Status: StatusOK,
				Detail: fmt.Sprintf("200, %d KB", len(body)/1024)},
		)
		// The Parse layer can only be verified if the canary is an ALBUM
		// page. The site root carries no albumFiles; calling that a FAIL makes
		// doctor a liar and sends the user in the wrong direction ("parse broke").
		if !strings.Contains(u.Path, "/a/") {
			out = append(out,
				LayerResult{Layer: LayerParse, Status: StatusWarn,
					Detail:   "no album canary, parsing not verified",
					Evidence: "add an /a/<id> URL to canary_urls"},
				LayerResult{Layer: LayerItemPage, Status: StatusWarn,
					Detail: "no album canary, API chain not verified"},
				LayerResult{Layer: LayerCDN, Status: StatusWarn,
					Detail: "no album canary, no CDN host seen"},
			)
		} else if files, perr := parseAlbumFiles(string(body)); perr != nil {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusFail,
				Detail: "could not parse albumFiles", Evidence: perr.Error()})
		} else {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusOK,
				Detail: fmt.Sprintf("albumFiles found %d items", len(files))})
			out = append(out, b.diagnoseItemAndCDN(ctx, files)...)
		}

	default:
		reason, burnable := burnReason(err)
		layer := LayerFetch
		if l, ok := LayerOf(err); ok {
			layer = l
		}
		var ch *challengeError
		switch {
		case errors.As(err, &ch):
			layer = LayerChallenge
		case burnable && strings.Contains(reason, "certificate"):
			layer = LayerTLS
		case burnable && strings.Contains(reason, "timeout"):
			layer = LayerTLS
		}
		detail := "request failed"
		if burnable {
			detail = reason
		}
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "DNS passed"},
			LayerResult{Layer: layer, Status: StatusFail, Detail: detail, Evidence: err.Error()},
		)
	}
	return out
}

// diagnoseItemAndCDN verifies the API chain and the CDN host when there is an
// album canary.
//
// A SINGLE item is resolved. doctor is a diagnostic tool; firing 200 API
// calls for 200 items would turn diagnosis into punishment and trigger the
// rate limit with our own hands.
func (b *bunkr) diagnoseItemAndCDN(ctx context.Context, files []bunkrFile) []LayerResult {
	var out []LayerResult

	fileURL, _, err := b.resolveFileURL(ctx, files[0].ID)
	if err != nil {
		// The error's OWN layer is kept. Writing every failure to ItemPage
		// sent the diagnosis to the wrong place: even when the signing
		// service crashed, the user started fiddling with api_endpoint.
		layer := LayerItemPage
		if l, ok := LayerOf(err); ok {
			layer = l
		}
		fail := LayerResult{Layer: layer, Status: StatusFail,
			Detail: "API chain broken", Evidence: collapseSpace(err.Error())}
		if layer == LayerCDN || layer == LayerChallenge {
			return append(out, fail)
		}
		return append(out, fail,
			LayerResult{Layer: LayerCDN, Status: StatusWarn,
				Detail: "the previous layer broke, so no CDN host could be seen"},
		)
	}
	out = append(out, LayerResult{Layer: LayerItemPage, Status: StatusOK,
		Detail: fmt.Sprintf("1/%d items resolved (sampling)", len(files))})

	host := ""
	if u, perr := url.Parse(fileURL); perr == nil {
		host = u.Host
	}
	switch {
	case host == "":
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusFail,
			Detail: "no host in the resolved URL", Evidence: fileURL})
	case MatchHost(host, b.cfg.CDNPatterns):
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusOK,
			Detail: "known CDN host", Evidence: host})
	default:
		// The CDN is a SIGNAL, NOT A GATE. An unknown host doesn't stop the
		// download; bunkr hosts rotate in normal operation. Making it a gate
		// would be producing the very breakage the tool later diagnoses.
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusWarn,
			Detail:   "new CDN host, not in cdn_patterns",
			Evidence: host + " (add it to sites.toml)"})
	}
	return out
}
