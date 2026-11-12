package site

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// DropboxName is the registry key.
const DropboxName = "dropbox"

// NewDropbox is the factory given to the registry.
//
// How Dropbox works (measured 2026-11-12), for shared links:
//
//   - A file link (/scl/fi/<key>/<name>?rlkey=…, or the old /s/<key>/<name>)
//     with dl=1 redirects to a download server (*.dl.dropboxusercontent.com)
//     that sends the file named in Content-Disposition, takes Range
//     requests and gives an ETag: several connections and resuming are
//     safe. Without dl=1 a browser gets the preview page.
//   - A folder link (/scl/fo/…, or the old /sh/…) with dl=1 gives the whole
//     folder as one zip built on the fly: its size in Content-Length, but a
//     Range request gets the whole zip again (200), so it comes over one
//     connection and an interrupted one starts over. With "unpack archives"
//     on it is unpacked when done.
//   - A link that is removed or wrong answers 200 with an error page.
func NewDropbox(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		cfg.Logln("dropbox: unrecognized extra key %q in config — ignored (possibly a typo)", k)
	}
	return &dropbox{cfg: cfg}
}

type dropbox struct {
	cfg SiteConfig
}

// dbLink is a parsed Dropbox link: the link to download (dl=1) and the one
// to show and resolve again (without dl).
type dbLink struct {
	folder   bool
	download string
	page     string
}

func (d *dropbox) Match(u string) bool {
	_, err := d.parse(u)
	return err == nil
}

// parse accepts shared file and folder links, old and new, and a link to
// a download server (taken as it is: it may be all the user has).
func (d *dropbox) parse(raw string) (dbLink, error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return dbLink{}, err
	}
	if MatchHost(host, d.cfg.CDNPatterns) {
		if len(splitPath(u.Path)) == 0 {
			return dbLink{}, fmt.Errorf("unsupported link: %s", u.Path)
		}
		return dbLink{download: u.String(), page: u.String()}, nil
	}
	if !MatchHost(host, d.cfg.Domains) && !MatchHost(host, d.cfg.MatchPatterns) {
		return dbLink{}, fmt.Errorf("unknown host: %s", host)
	}
	seg := splitPath(u.Path)
	var folder bool
	switch {
	case len(seg) >= 3 && seg[0] == "s":
	case len(seg) >= 4 && seg[0] == "scl" && seg[1] == "fi":
	case len(seg) >= 3 && seg[0] == "sh":
		folder = true
	case len(seg) >= 3 && seg[0] == "scl" && seg[1] == "fo":
		folder = true
	default:
		return dbLink{}, fmt.Errorf("unsupported path: %s", u.Path)
	}
	q := u.Query()
	q.Del("dl")
	q.Del("raw")
	q.Del("preview") // a folder link pointing at one file still means the folder
	page := url.URL{Scheme: "https", Host: "www.dropbox.com", Path: u.Path, RawQuery: q.Encode()}
	q.Set("dl", "1")
	download := page
	download.RawQuery = q.Encode()
	return dbLink{folder: folder, download: download.String(), page: page.String()}, nil
}

func (d *dropbox) client() *http.Client {
	if d.cfg.HTTPClient != nil {
		return d.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (d *dropbox) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	it, err := d.ResolveOne(ctx, u)
	if err != nil {
		return nil, err
	}
	return nil, yield(it)
}

// ResolveOne asks for the first byte: the name, the size, and that the link
// still gives a file.
func (d *dropbox) ResolveOne(ctx context.Context, u string) (Item, error) {
	l, err := d.parse(u)
	if err != nil {
		return Item{}, Errorf(LayerParse, u, "%v", err)
	}
	it, err := probeFile(ctx, d.client(), d.cfg.UserAgent, l.download)
	if errors.Is(err, errWebPage) {
		return Item{}, Errorf(LayerItemPage, l.page,
			"Dropbox answered with a page, not the file: the link was removed, is wrong, or needs a password")
	}
	if err != nil {
		return Item{}, err
	}
	it.SourcePage = l.page
	return it, nil
}

// ValidateResponse refuses Dropbox's error page where the file should be.
// A page sent as an attachment is a file someone shared.
func (d *dropbox) ValidateResponse(resp *http.Response) error {
	if !isPage(resp) {
		return nil
	}
	if attachment, _ := disposition(resp.Header.Get("Content-Disposition")); attachment {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: errors.New("Dropbox answered with a page, not the file: the link was removed or is wrong")}
}

// Diagnose asks for the first byte of the canary file: the shared link
// answering (Fetch, Parse, ItemPage) and the download server it leads to
// (CDN).
func (d *dropbox) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, d.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		it, err := d.ResolveOne(ctx, canary)
		if err != nil {
			return nil, err
		}
		final, err := d.finalHost(ctx, it.URL)
		if err != nil {
			return nil, err
		}
		return []LayerResult{
			{Layer: LayerFetch, Status: StatusOK, Detail: "the shared link answered"},
			{Layer: LayerParse, Status: StatusOK, Detail: fmt.Sprintf("%s, %d bytes", it.Filename, it.Size)},
			{Layer: LayerItemPage, Status: StatusOK, Detail: "dl=1 gives the file"},
			cdnResult(d.cfg, final),
		}, nil
	})
}

// finalHost is where a download link ends up after its redirects.
func (d *dropbox) finalHost(ctx context.Context, link string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", Errorf(LayerFetch, link, "%v", err)
	}
	if d.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", d.cfg.UserAgent)
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := d.client().Do(req)
	if err != nil {
		return "", Errorf(LayerCDN, link, "%w", err)
	}
	resp.Body.Close()
	return resp.Request.URL.String(), nil
}
