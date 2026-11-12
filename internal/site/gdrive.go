package site

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// GdriveName is the registry key.
const GdriveName = "gdrive"

// NewGdrive is the factory given to the registry.
//
// How Google Drive works (measured 2026-11-12), for files shared with
// "anyone with the link", without an account:
//
//   - A file comes from drive.usercontent.google.com/download?id=<id>&
//     export=download&confirm=t. confirm=t skips the "can't scan this file
//     for viruses" page large files get (a 476 MB file came straight away,
//     no uuid needed). The answer names the file (Content-Disposition),
//     takes Range requests, gives the size in Content-Range and a
//     Last-Modified (no ETag) that If-Range honours: several connections
//     and resuming are safe. About 4 MB/s per connection.
//   - A missing file is a 404 page; one not shared, or whose download
//     quota is used up, is a page too, never the file.
//   - A folder is listed by drive.google.com/embeddedfolderview?id=<id>: a
//     page with the folder's name and, for each entry, its link (a file, a
//     subfolder, or a Google Docs/Sheets/Slides document) and name; no
//     sizes.
//   - A document is exported: docs.google.com/<kind>/d/<id>/export?format=
//     docx, xlsx or pptx, named by the server, in one piece (no Range).
func NewGdrive(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		cfg.Logln("gdrive: unrecognized extra key %q in config — ignored (possibly a typo)", k)
	}
	return &gdrive{cfg: cfg, w: web{cfg: cfg}}
}

type gdrive struct {
	cfg SiteConfig
	w   web
}

type gdKind int

const (
	gdFile gdKind = iota
	gdFolder
	gdDoc
)

// gdLink is a parsed Drive link; doc is the document kind for gdDoc
// ("document", "spreadsheets", "presentation").
type gdLink struct {
	kind gdKind
	id   string
	doc  string
}

// gdExport is each document kind's export format.
var gdExport = map[string]string{"document": "docx", "spreadsheets": "xlsx", "presentation": "pptx"}

// gdMaxDepth bounds how deep subfolders are followed.
const gdMaxDepth = 20

func (g *gdrive) Match(u string) bool {
	_, err := g.parse(u)
	return err == nil
}

// parse accepts the forms Drive links come in:
//
//	drive.google.com/file/d/<id>/view       a file
//	drive.google.com/open?id=<id>           a file
//	drive.google.com/uc?id=<id>             a file
//	drive.usercontent.google.com/download?id=<id>
//	drive.google.com/drive/folders/<id>     a folder (also /drive/u/0/...)
//	drive.google.com/folderview?id=<id>     a folder
//	docs.google.com/document/d/<id>/edit    a document (spreadsheets, presentation)
func (g *gdrive) parse(raw string) (gdLink, error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return gdLink{}, err
	}
	if !MatchHost(host, g.cfg.Domains) && !MatchHost(host, g.cfg.MatchPatterns) {
		return gdLink{}, fmt.Errorf("unknown host: %s", host)
	}
	seg := splitPath(u.Path)
	qid := u.Query().Get("id")
	switch {
	case len(seg) >= 3 && seg[0] == "file" && seg[1] == "d" && gdID(seg[2]):
		return gdLink{kind: gdFile, id: seg[2]}, nil
	case len(seg) == 1 && (seg[0] == "open" || seg[0] == "uc" || seg[0] == "download") && gdID(qid):
		return gdLink{kind: gdFile, id: qid}, nil
	case len(seg) == 1 && (seg[0] == "folderview" || seg[0] == "embeddedfolderview") && gdID(qid):
		return gdLink{kind: gdFolder, id: qid}, nil
	case len(seg) >= 3 && gdExport[seg[0]] != "" && seg[1] == "d" && gdID(seg[2]):
		return gdLink{kind: gdDoc, id: seg[2], doc: seg[0]}, nil
	}
	// drive/folders/<id>, drive/u/<n>/folders/<id>, drive/mobile/folders/<id>
	for i := 0; i+1 < len(seg); i++ {
		if seg[0] == "drive" && seg[i] == "folders" && gdID(seg[i+1]) {
			return gdLink{kind: gdFolder, id: seg[i+1]}, nil
		}
	}
	return gdLink{}, fmt.Errorf("unsupported path: %s", u.Path)
}

// gdID: Drive ids are long runs of letters, digits, - and _.
func gdID(s string) bool {
	if len(s) < 10 || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func gdFileView(id string) string { return "https://drive.google.com/file/d/" + id + "/view" }

func gdFileURL(id string) string {
	return "https://drive.usercontent.google.com/download?id=" + url.QueryEscape(id) + "&export=download&confirm=t"
}

func gdDocPage(doc, id string) string { return "https://docs.google.com/" + doc + "/d/" + id + "/edit" }

func gdDocURL(doc, id string) string {
	return "https://docs.google.com/" + doc + "/d/" + id + "/export?format=" + gdExport[doc]
}

func gdFolderURL(id string) string {
	return "https://drive.google.com/embeddedfolderview?id=" + url.QueryEscape(id)
}

func (g *gdrive) client() *http.Client {
	if g.cfg.HTTPClient != nil {
		return g.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (g *gdrive) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	l, err := g.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if l.kind == gdFolder {
		return g.resolveFolder(ctx, l.id, yield)
	}
	it, err := g.resolveOne(ctx, l)
	if err != nil {
		return nil, err
	}
	return nil, yield(it)
}

// resolveOne asks for the first byte of a file or an export: it says the
// name and size, and that the link really gives the file.
func (g *gdrive) resolveOne(ctx context.Context, l gdLink) (Item, error) {
	link, page := gdFileURL(l.id), gdFileView(l.id)
	if l.kind == gdDoc {
		link, page = gdDocURL(l.doc, l.id), gdDocPage(l.doc, l.id)
	}
	it, err := probeFile(ctx, g.client(), g.cfg.UserAgent, link)
	if errors.Is(err, errWebPage) {
		return Item{}, Errorf(LayerItemPage, page,
			"Google Drive answered with a page, not the file: it was removed, isn't shared with anyone who has the link, or too many people downloaded it today (try later)")
	}
	if err != nil {
		return Item{}, err
	}
	it.SourcePage = page
	return it, nil
}

var (
	gdTitleRe     = regexp.MustCompile(`<title>([^<]*)</title>`)
	gdHrefRe      = regexp.MustCompile(`<a href="([^"]+)"`)
	gdEntryNameRe = regexp.MustCompile(`<div class="flip-entry-title">([^<]*)</div>`)
)

// gdEntryStart begins each entry of the folder view.
const gdEntryStart = `<div class="flip-entry" id="entry-`

type gdEntry struct {
	link gdLink
	name string
}

// gdFolderPage reads the embedded folder view: the folder's name and its
// entries, each taken by its link (a file, a subfolder or a document).
// Entries whose link isn't one of those are counted in skipped.
func (g *gdrive) gdFolderPage(page string) (title string, entries []gdEntry, skipped int) {
	if m := gdTitleRe.FindStringSubmatch(page); m != nil {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	chunks := strings.Split(page, gdEntryStart)
	for _, c := range chunks[1:] {
		href, nm := gdHrefRe.FindStringSubmatch(c), gdEntryNameRe.FindStringSubmatch(c)
		if href == nil || nm == nil {
			skipped++
			continue
		}
		name := strings.TrimSpace(html.UnescapeString(nm[1]))
		l, err := g.parse(html.UnescapeString(href[1]))
		if err != nil || name == "" {
			skipped++
			continue
		}
		entries = append(entries, gdEntry{link: l, name: name})
	}
	return title, entries, skipped
}

// listFolder fetches and reads a folder's view.
func (g *gdrive) listFolder(ctx context.Context, id string) (string, []gdEntry, int, error) {
	pageURL := gdFolderURL(id)
	resp, body, err := g.w.get(ctx, pageURL, nil)
	if err != nil {
		return "", nil, 0, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil, 0, Errorf(LayerFetch, pageURL, "the folder doesn't exist, or isn't shared with anyone who has the link")
	case resp.StatusCode != http.StatusOK:
		return "", nil, 0, statusError(pageURL, resp, body)
	}
	g.cfg.Recordln("folder.html", body)
	title, entries, skipped := g.gdFolderPage(string(body))
	if title == "" && len(entries) == 0 && !strings.Contains(string(body), "flip-entries") {
		return "", nil, 0, Errorf(LayerParse, pageURL, "no folder on the page (it may not be shared, or its layout changed)")
	}
	return title, entries, skipped, nil
}

type gdWalk struct {
	g        *gdrive
	yield    func(Item) error
	seen     map[string]bool
	index    int
	itemErrs []ItemError
	stopped  bool
}

func (g *gdrive) resolveFolder(ctx context.Context, id string, yield func(Item) error) ([]ItemError, error) {
	w := &gdWalk{g: g, yield: yield, seen: map[string]bool{}}
	if err := w.folder(ctx, id, "", 0); err != nil {
		return w.itemErrs, err
	}
	if w.index == 0 && len(w.itemErrs) == 0 {
		return nil, Errorf(LayerParse, gdFolderURL(id), "the folder has no files")
	}
	return w.itemErrs, nil
}

// folder yields one folder's files and documents, then walks its
// subfolders; dir is "" for the root, named after the folder itself.
func (w *gdWalk) folder(ctx context.Context, id, dir string, depth int) error {
	if w.seen[id] {
		return nil
	}
	w.seen[id] = true
	if depth > gdMaxDepth {
		return Errorf(LayerParse, id, "folders nested deeper than %d levels", gdMaxDepth)
	}
	title, entries, skipped, err := w.g.listFolder(ctx, id)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = sanitizeDirLabel(title, id)
	}
	if skipped > 0 {
		w.itemErrs = append(w.itemErrs, ItemError{URL: gdFolderURL(id),
			Err: Errorf(LayerParse, fmt.Sprintf("%d entries", skipped), "entries of a kind Siphon can't download (forms, shortcuts…) were left out")})
	}
	var subs []gdEntry
	for _, e := range entries {
		var it Item
		switch e.link.kind {
		case gdFolder:
			subs = append(subs, e)
			continue
		case gdFile:
			it = Item{URL: gdFileURL(e.link.id), SourcePage: gdFileView(e.link.id), Filename: e.name}
		case gdDoc:
			name := e.name
			if ext := "." + gdExport[e.link.doc]; !strings.HasSuffix(strings.ToLower(name), ext) {
				name += ext
			}
			it = Item{URL: gdDocURL(e.link.doc, e.link.id), SourcePage: gdDocPage(e.link.doc, e.link.id), Filename: name}
		}
		it.Dir, it.Size, it.Index = dir, -1, w.index
		w.index++
		if err := w.yield(it); err != nil {
			w.stopped = true
			return err
		}
	}
	for _, sub := range subs {
		serr := w.folder(ctx, sub.link.id, dir+"/"+sanitizeDirLabel(sub.name, sub.link.id), depth+1)
		if serr == nil {
			continue
		}
		if w.stopped || ctx.Err() != nil {
			return serr
		}
		// One subfolder that can't be listed doesn't sink the rest.
		w.itemErrs = append(w.itemErrs, ItemError{URL: gdFolderURL(sub.link.id), Err: serr})
	}
	return nil
}

// ResolveOne resolves a file or document again: a folder's entries are
// queued without a size, which this fills in.
func (g *gdrive) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	l, err := g.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if l.kind == gdFolder {
		return Item{}, Errorf(LayerParse, sourcePage, "expected a file, got a folder")
	}
	return g.resolveOne(ctx, l)
}

// ValidateResponse refuses a page where the file should be: Drive answers
// a file it won't give (no longer shared, its download quota used up) with
// a page. A page sent as an attachment is a file someone uploaded.
func (g *gdrive) ValidateResponse(resp *http.Response) error {
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
		Err: errors.New("Google Drive answered with a page, not the file: it isn't shared any more, or too many people downloaded it today (try later)")}
}

// Diagnose lists the canary folder (Fetch, Parse) and asks for the first
// byte of its first file (ItemPage, CDN).
func (g *gdrive) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, g.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		l, err := g.parse(canary)
		if err != nil || l.kind != gdFolder {
			return nil, Errorf(LayerParse, canary, "the canary must be a Google Drive folder link")
		}
		title, entries, _, err := g.listFolder(ctx, l.id)
		if err != nil {
			return nil, err
		}
		out := []LayerResult{
			{Layer: LayerFetch, Status: StatusOK, Detail: "folder view answered"},
			{Layer: LayerParse, Status: StatusOK, Detail: fmt.Sprintf("folder %q, %d entries", title, len(entries))},
		}
		for _, e := range entries {
			if e.link.kind != gdFile {
				continue
			}
			it, err := g.resolveOne(ctx, e.link)
			if err != nil {
				return out, err
			}
			return append(out,
				LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: fmt.Sprintf("%s: the file answered (%d bytes)", it.Filename, it.Size)},
				cdnResult(g.cfg, it.URL)), nil
		}
		return out, Errorf(LayerItemPage, canary, "the canary folder has no file to check")
	})
}
