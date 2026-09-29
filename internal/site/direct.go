package site

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/external"
)

// DirectName is the "site" for plain file links: any http(s) URL that no
// real site recognizes, such as https://example.com/files/setup.zip. A link
// that turns out to be a web page goes to yt-dlp or gallery-dl, if installed.
const DirectName = "direct"

// Fallback is an optional interface for resolvers that match broadly and
// must only get a URL when no other resolver recognizes it. run.Pick asks
// them last, whatever their position in sites.toml.
type Fallback interface {
	Fallback() bool
}

// SelfDownloader is an optional interface for resolvers that download some
// items themselves instead of handing a URL to the downloader: an external
// tool that does its own extraction, format choice and merging.
type SelfDownloader interface {
	// Handles reports whether the item is downloaded by the resolver.
	Handles(it Item) bool
	// DownloadSelf downloads it into dir; progress may be nil and total is
	// -1 when unknown. It returns the file (or, for a gallery, the folder)
	// and the bytes written.
	DownloadSelf(ctx context.Context, dir string, it Item, progress func(done, total int64)) (path string, size int64, err error)
}

// Links handed to an external tool carry the tool's name in front
// ("yt-dlp:https://…"), in the queue and in the ledger: after a restart the
// job goes straight back to that tool without probing the page again. A
// user can type the prefix too, to send a link to a tool on purpose.
const (
	ViaYtDlp     = "yt-dlp:"
	ViaGalleryDL = "gallery-dl:"
)

// viaTool splits a tool prefix off a link; prefix is "" for a plain link.
func viaTool(u string) (prefix, link string) {
	u = strings.TrimSpace(u)
	for _, p := range []string{ViaYtDlp, ViaGalleryDL} {
		if strings.HasPrefix(u, p) {
			return p, strings.TrimSpace(strings.TrimPrefix(u, p))
		}
	}
	return "", u
}

// errWebPage marks "the link is a web page, not a file".
var errWebPage = errors.New("the link opens a web page, not a file")

// directExtra are the sites.toml [site.extra] keys direct understands: where
// the external tools are, when they aren't next to Siphon or on PATH.
var directExtra = map[string]bool{"yt_dlp": true, "gallery_dl": true, "ffmpeg": true, "deno": true}

// direct resolves a plain file link into a single item. Nothing site
// specific: one small request (the first byte) tells whether the link is a
// file at all, its name and its size. From there the ordinary downloader
// takes over, with resume and several connections when the server allows.
//
// A link that turns out to be a web page may still be a video or a gallery
// a tool knows: it goes to yt-dlp, then gallery-dl, when they are installed.
type direct struct {
	cfg SiteConfig
	// owned: host patterns of the real sites (their domains and CDNs).
	owned []string
}

func NewDirect(cfg SiteConfig) Resolver {
	for k := range cfg.Extra {
		if !directExtra[k] {
			cfg.Logln("direct: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}
	return &direct{cfg: cfg}
}

func (d *direct) Fallback() bool { return true }

// ExcludeHosts implements HostExcluder: links on these hosts belong to a real
// site and are never taken as plain files, even when that site doesn't
// recognize them. A mega link with a truncated key would otherwise be saved
// as mega's web page, and a mega storage URL as encrypted bytes.
func (d *direct) ExcludeHosts(patterns []string) { d.owned = patterns }

// Match takes any http(s) URL that isn't a real site's, and any link with a
// tool prefix; Pick only asks after every real site said no.
func (d *direct) Match(u string) bool {
	prefix, link := viaTool(u)
	p, err := url.Parse(link)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return false
	}
	// A tool prefix is the user's explicit choice; it overrides the rule.
	return prefix != "" || !MatchHost(p.Hostname(), d.owned)
}

// HostExcluder is an optional interface for fallback resolvers: run.Setup
// hands them the real sites' host patterns to stay away from.
type HostExcluder interface {
	ExcludeHosts(patterns []string)
}

func (d *direct) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	var items []Item
	prefix, link := viaTool(u)
	if prefix != "" {
		var err error
		if items, err = d.viaTools(ctx, link, prefix); err != nil {
			return nil, err
		}
	} else {
		it, err := d.probeFile(ctx, link)
		switch {
		case err == nil:
			items = []Item{it}
		case errors.Is(err, errWebPage):
			// Not a file: maybe a video or gallery page a tool knows.
			if items, err = d.viaTools(ctx, link, ""); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	for _, it := range items {
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// viaTools asks the external tools about a web page: yt-dlp first (video
// and audio: one item per video, a playlist becomes a folder), then
// gallery-dl (one item for the whole gallery). only limits it to one tool.
func (d *direct) viaTools(ctx context.Context, link, only string) ([]Item, error) {
	tools := external.Find(d.cfg.Extra)
	if tools.YtDlp == "" && tools.GalleryDL == "" || only == ViaYtDlp && tools.YtDlp == "" || only == ViaGalleryDL && tools.GalleryDL == "" {
		what := "yt-dlp or gallery-dl"
		switch only {
		case ViaYtDlp:
			what = "yt-dlp"
		case ViaGalleryDL:
			what = "gallery-dl"
		}
		return nil, Errorf(LayerParse, link,
			"%w, and no supported site recognizes it; for video and gallery sites install %s (Diagnose > Install tools, or 'siphon tools install') and Siphon will use it",
			errWebPage, what)
	}

	// yt-dlp's reason when it knew the site but couldn't get the link
	// (private, removed, or "not a video"): kept for the error if gallery-dl
	// can't take the link either.
	var ytErr error
	if only != ViaGalleryDL && tools.YtDlp != "" {
		d.cfg.Logln("asking yt-dlp about %s", link)
		playlist, entries, err := tools.YtDlpProbe(ctx, link)
		switch {
		case err == nil:
			items := make([]Item, 0, len(entries))
			for i, e := range entries {
				items = append(items, Item{
					URL:        e.URL,
					SourcePage: ViaYtDlp + e.URL,
					Dir:        playlist,
					Filename:   videoName(e),
					Size:       e.Size,
					Index:      i,
				})
			}
			return items, nil
		case !errors.Is(err, external.ErrUnsupported):
			// yt-dlp knows the site but couldn't get the link. MEASURED: an
			// imgur image page is claimed by yt-dlp's imgur extractor and
			// fails with "not a video or animated image"; gallery-dl takes
			// it. So gallery-dl is still asked, and yt-dlp's reason (private,
			// removed, region locked…) is the answer only if it can't.
			ytErr = err
		}
	}

	if only != ViaYtDlp && tools.GalleryDL != "" {
		d.cfg.Logln("asking gallery-dl about %s", link)
		err := tools.GalleryDLProbe(ctx, link)
		switch {
		case err == nil:
			return []Item{{URL: link, SourcePage: ViaGalleryDL + link, Filename: galleryName(link), Size: -1}}, nil
		case ytErr == nil && !errors.Is(err, external.ErrUnsupported):
			return nil, Errorf(LayerParse, link, "%w", err)
		}
	}
	if ytErr != nil {
		return nil, Errorf(LayerParse, link, "%w", ytErr)
	}

	var tried []string
	if tools.YtDlp != "" && only != ViaGalleryDL {
		tried = append(tried, "yt-dlp")
	}
	if tools.GalleryDL != "" && only != ViaYtDlp {
		tried = append(tried, "gallery-dl")
	}
	return nil, Errorf(LayerParse, link, "%w, and neither Siphon nor %s recognizes it",
		errWebPage, strings.Join(tried, " nor "))
}

// videoName is a queue name for a video before yt-dlp picks the real one
// ("<title> [<id>]"; the extension depends on the format it chooses).
func videoName(e external.Entry) string {
	switch {
	case e.Title != "" && e.ID != "":
		return e.Title + " [" + e.ID + "]"
	case e.Title != "":
		return e.Title
	case e.ID != "":
		return e.ID
	}
	return "video"
}

// galleryName is a queue name for a gallery: the host and the path, e.g.
// "imgur.com a 1abc".
func galleryName(link string) string {
	p, err := url.Parse(link)
	if err != nil {
		return "gallery"
	}
	parts := []string{strings.TrimPrefix(p.Hostname(), "www.")}
	for _, s := range strings.Split(p.Path, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// Handles implements SelfDownloader: links handed to a tool.
func (d *direct) Handles(it Item) bool {
	prefix, _ := viaTool(it.SourcePage)
	return prefix != ""
}

// DownloadSelf implements SelfDownloader: the tool downloads the link itself.
func (d *direct) DownloadSelf(ctx context.Context, dir string, it Item, progress func(done, total int64)) (string, int64, error) {
	tools := external.Find(d.cfg.Extra)
	prefix, link := viaTool(it.SourcePage)
	switch {
	case prefix == ViaYtDlp && tools.YtDlp != "":
		return tools.YtDlpDownload(ctx, link, dir, progress)
	case prefix == ViaGalleryDL && tools.GalleryDL != "":
		return tools.GalleryDLDownload(ctx, link, dir, progress)
	case prefix != "":
		return "", 0, fmt.Errorf("%s isn't installed any more (Diagnose > Install tools, or 'siphon tools install')", strings.TrimSuffix(prefix, ":"))
	}
	return "", 0, fmt.Errorf("not a tool link: %s", it.SourcePage)
}

// ResolveOne resolves a single link again (after a restart). A tool link
// needs nothing: the tool does its own extraction when it downloads.
func (d *direct) ResolveOne(ctx context.Context, u string) (Item, error) {
	if prefix, link := viaTool(u); prefix != "" {
		return Item{URL: link, SourcePage: prefix + link, Filename: galleryName(link), Size: -1}, nil
	}
	return d.probeFile(ctx, strings.TrimSpace(u))
}

// probeFile asks for the first byte of the link. A Range request rather
// than HEAD: plenty of servers answer HEAD differently from GET (or not at
// all), and the answer also shows whether ranges work.
//
// A link that answers with a web page is not a file (errWebPage): saving a
// login or "file not found" page and calling it a success would be exactly
// the silent failure this tool is built to avoid. A page sent as an
// attachment is a file, though.
func (d *direct) probeFile(ctx context.Context, u string) (Item, error) {
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

	attachment, cdName := disposition(resp.Header.Get("Content-Disposition"))
	ctype := resp.Header.Get("Content-Type")
	switch {
	case isPageType(ctype) && !attachment && resp.StatusCode < 500:
		// A page, whatever its status: a video site answering 404 or 403 to
		// a plain client still leaves the decision to the tools.
		return Item{}, Errorf(LayerParse, u, "%w (%s)", errWebPage, mediaType(ctype))
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return Item{}, Errorf(LayerFetch, u, "the file doesn't exist (HTTP %d)", resp.StatusCode)
	default:
		return Item{}, Errorf(LayerFetch, u, "the server refused the link: HTTP %s", resp.Status)
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
