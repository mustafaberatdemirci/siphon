package site

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// DirectName is the "site" for plain file links: any http(s) URL that no
// real site recognizes, such as https://example.com/files/setup.zip.
const DirectName = "direct"

// Fallback is an optional interface for resolvers that match broadly and
// must only get a URL when no other resolver recognizes it. run.Pick asks
// them last, whatever their position in sites.toml.
type Fallback interface {
	Fallback() bool
}

// direct resolves a plain file link into a single item. Nothing site
// specific: one small request (the first byte) tells whether the link is a
// file at all, its name and its size. From there the ordinary downloader
// takes over, with resume and several connections when the server allows.
type direct struct {
	cfg SiteConfig
	// owned: host patterns of the real sites (their domains and CDNs).
	owned []string
}

func NewDirect(cfg SiteConfig) Resolver { return &direct{cfg: cfg} }

func (d *direct) Fallback() bool { return true }

// ExcludeHosts implements HostExcluder: links on these hosts belong to a real
// site and are never taken as plain files, even when that site doesn't
// recognize them. A mega link with a truncated key would otherwise be saved
// as mega's web page, and a mega storage URL as encrypted bytes.
func (d *direct) ExcludeHosts(patterns []string) { d.owned = patterns }

// Match takes any http(s) URL that isn't a real site's; Pick only asks after
// every real site said no.
func (d *direct) Match(u string) bool {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return false
	}
	return !MatchHost(p.Hostname(), d.owned)
}

// HostExcluder is an optional interface for fallback resolvers: run.Setup
// hands them the real sites' host patterns to stay away from.
type HostExcluder interface {
	ExcludeHosts(patterns []string)
}

func (d *direct) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	it, err := d.ResolveOne(ctx, u)
	if err != nil {
		return nil, err
	}
	return nil, yield(it)
}

// ResolveOne asks for the first byte of the link. A Range request rather
// than HEAD: plenty of servers answer HEAD differently from GET (or not at
// all), and the answer also shows whether ranges work.
//
// A link that answers with a web page is refused: downloading a login or
// "file not found" page and calling it a success would be exactly the silent
// failure this tool is built to avoid. A page sent as an attachment is a
// file, though.
func (d *direct) ResolveOne(ctx context.Context, u string) (Item, error) {
	u = strings.TrimSpace(u)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Item{}, Errorf(LayerFetch, u, "invalid link: %w", err)
	}
	if d.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", d.cfg.UserAgent)
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := d.client().Do(req)
	if err != nil {
		// Wrapped, not flattened: the queue tells "the network is down" from
		// a real answer by the error's type.
		return Item{}, Errorf(LayerFetch, u, "%w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusNotFound, http.StatusGone:
		return Item{}, Errorf(LayerFetch, u, "the file doesn't exist (HTTP %d)", resp.StatusCode)
	default:
		return Item{}, Errorf(LayerFetch, u, "the server refused the link: HTTP %s", resp.Status)
	}

	attachment, cdName := disposition(resp.Header.Get("Content-Disposition"))
	ctype := resp.Header.Get("Content-Type")
	if isPageType(ctype) && !attachment {
		return Item{}, Errorf(LayerParse, u,
			"the link opens a web page (%s), not a file, and no supported site recognizes it", mediaType(ctype))
	}

	final := u
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String() // after redirects
	}
	return Item{
		URL:        u,
		SourcePage: u,
		Filename:   directFilename(cdName, final, u, ctype),
		Size:       directSize(resp),
	}, nil
}

// Diagnose: there is no site to diagnose; every link is checked when it is
// added. doctor leaves fallbacks out.
func (d *direct) Diagnose(context.Context) ([]LayerResult, error) { return nil, nil }

func (d *direct) client() *http.Client {
	if d.cfg.HTTPClient != nil {
		return d.cfg.HTTPClient
	}
	return http.DefaultClient
}

// disposition reads a Content-Disposition header: whether it says
// "attachment", and the file name it gives (mime decodes the RFC 5987
// filename*=UTF-8”… form too, and prefers it).
func disposition(h string) (attachment bool, name string) {
	if h == "" {
		return false, ""
	}
	kind, params, err := mime.ParseMediaType(h)
	if err != nil {
		return false, ""
	}
	return strings.EqualFold(kind, "attachment"), params["filename"]
}

// isPageType: HTML is a page. Other types (even text or JSON) can be files
// someone links to on purpose.
func isPageType(ctype string) bool {
	switch mediaType(ctype) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

func mediaType(ctype string) string {
	mt, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ctype))
	}
	return mt
}

// directSize is the file's size from a first-byte answer: Content-Range's
// total for 206, Content-Length for 200; -1 when the server doesn't say.
func directSize(resp *http.Response) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range") // "bytes 0-0/12345"
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil && n > 0 {
				return n
			}
		}
		return -1
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return -1
}

// directFilename picks the name: the server's Content-Disposition first, then
// the last path segment of the URL after redirects ("/download?id=7"
// redirecting to "/files/setup.zip"), then of the link itself, then the host.
// A name without an extension gets one from the Content-Type when that is
// known. The downloader sanitizes it for the disk.
func directFilename(cdName, final, orig, ctype string) string {
	name := strings.TrimSpace(cdName)
	for _, raw := range []string{final, orig} {
		if name != "" {
			break
		}
		if p, err := url.Parse(raw); err == nil {
			if base := path.Base(p.Path); base != "." && base != "/" {
				name = base
			}
		}
	}
	if name == "" {
		if p, err := url.Parse(orig); err == nil && p.Hostname() != "" {
			name = p.Hostname()
		} else {
			name = "download"
		}
	}
	if !strings.Contains(name, ".") {
		if exts, err := mime.ExtensionsByType(mediaType(ctype)); err == nil && len(exts) > 0 {
			name += exts[0]
		}
	}
	return name
}
