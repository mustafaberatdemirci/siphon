package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	cfFolderNode = regexp.MustCompile(`loadImages\('folder',\s*'(\d+)'`)
	cfEntryTag   = regexp.MustCompile(`<(?:div|li)\b[^>]*\b(?:fileId|folderId)="\d+"[^>]*>`)
	cfAttr       = regexp.MustCompile(`([a-zA-Z_:-]+)="([^"]*)"`)
	cfHidden     = regexp.MustCompile(`<input id="(rsp[A-Za-z]+)" value="([^"]*)" type="hidden"`)
)

// cfEntry is one entry of a folder listing: a file or a subfolder.
type cfEntry struct {
	folder bool
	id     string // fileId or folderId
	code   string // a file's page code
	name   string
	size   int64 // a file's exact size; -1 if not given
}

// folderPage is a folder's canonical page.
func (c *cyberfile) folderPage(hash string) string { return c.base() + "/folder/" + hash }

// folderNode reads a folder's numeric id from its page.
func (c *cyberfile) folderNode(ctx context.Context, hash string) (string, error) {
	page := c.folderPage(hash)
	resp, body, err := c.w.get(ctx, page, nil)
	if err != nil {
		return "", err
	}
	c.cfg.Recordln("folder.html", body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", Errorf(LayerItemPage, page, "the folder doesn't exist or was removed (HTTP 404)")
	case resp.StatusCode != http.StatusOK:
		return "", statusError(page, resp, body)
	}
	if m := cfFolderNode.FindSubmatch(body); m != nil {
		return string(m[1]), nil
	}
	// MEASURED: a folder hash the site doesn't know is redirected to its
	// account page.
	if resp.Request != nil && resp.Request.URL != nil && strings.HasPrefix(resp.Request.URL.Path, "/account") {
		return "", Errorf(LayerItemPage, page, "the folder doesn't exist or isn't shared (cyberfile sent its account page)")
	}
	if hasCaptcha(string(body)) {
		return "", &LayerError{Layer: LayerChallenge, Evidence: page,
			Err: captchaError{errors.New("cyberfile asks for a captcha; open the page in a browser, then resume")}}
	}
	return "", Errorf(LayerParse, page, "no folder id on the folder page (did its layout change?)")
}

// listPage asks for one page of a folder's entries and returns them, the
// number of pages and the folder's name.
func (c *cyberfile) listPage(ctx context.Context, node string, page int) ([]cfEntry, int, string, error) {
	form := url.Values{
		"pageType":      {"folder"},
		"nodeId":        {node},
		"pageStart":     {strconv.Itoa(page)},
		"perPage":       {strconv.Itoa(cyberfilePerPage)},
		"filterOrderBy": {""},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.list, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, "", Errorf(LayerFetch, c.list, "could not build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", c.base()+"/")
	resp, body, err := c.w.do(req)
	if err != nil {
		return nil, 0, "", err
	}
	c.cfg.Recordln("folder_list.json", body)
	if resp.StatusCode != http.StatusOK {
		return nil, 0, "", statusError(c.list, resp, body)
	}
	var d struct {
		HTML      string `json:"html"`
		PageTitle string `json:"page_title"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, 0, "", Errorf(LayerParse, c.list, "could not decode JSON: %v", err)
	}
	pages := 1
	for _, m := range cfHidden.FindAllStringSubmatch(d.HTML, -1) {
		if m[1] == "rspTotalPages" {
			if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
				pages = n
			}
		}
	}
	return parseCfEntries(d.HTML), pages, strings.TrimSpace(html.UnescapeString(d.PageTitle)), nil
}

// parseCfEntries reads a listing's entries: a file carries fileId, its page
// (dtfullurl), its name and its size in bytes (dtsizeraw); a subfolder
// carries folderId and its name.
func parseCfEntries(listing string) []cfEntry {
	var out []cfEntry
	for _, tag := range cfEntryTag.FindAllString(listing, -1) {
		a := map[string]string{}
		for _, m := range cfAttr.FindAllStringSubmatch(tag, -1) {
			a[m[1]] = html.UnescapeString(m[2])
		}
		name := strings.TrimSpace(a["dtfilename"])
		if name == "" {
			name = strings.TrimSpace(a["dttitle"])
		}
		switch {
		case a["fileId"] != "":
			code := ""
			if u, err := url.Parse(a["dtfullurl"]); err == nil {
				if seg := splitPath(u.Path); len(seg) >= 1 && cfCode(seg[0]) {
					code = seg[0]
				}
			}
			if code == "" {
				continue
			}
			size, err := strconv.ParseInt(a["dtsizeraw"], 10, 64)
			if err != nil || size <= 0 {
				size = -1
			}
			out = append(out, cfEntry{id: a["fileId"], code: code, name: name, size: size})
		case a["folderId"] != "":
			if name == "" {
				name = strings.TrimSpace(a["title"])
			}
			out = append(out, cfEntry{folder: true, id: a["folderId"], name: name})
		}
	}
	return out
}

// cfWalk resolves a folder tree: a folder's files first, then each
// subfolder under a folder of the same name.
type cfWalk struct {
	c        *cyberfile
	root     string // the root folder's page, for item errors
	yield    func(Item) error
	seen     map[string]bool // folder ids walked
	files    map[string]bool // file codes yielded
	index    int
	itemErrs []ItemError
	stopped  bool // yield asked to stop
}

func (c *cyberfile) resolveFolder(ctx context.Context, hash string, yield func(Item) error) ([]ItemError, error) {
	node, err := c.folderNode(ctx, hash)
	if err != nil {
		return nil, err
	}
	w := &cfWalk{c: c, root: c.folderPage(hash), yield: yield, seen: map[string]bool{node: true}, files: map[string]bool{}}
	if err := w.folder(ctx, node, "", 0); err != nil {
		return w.itemErrs, err
	}
	if w.index == 0 && len(w.itemErrs) == 0 {
		return nil, Errorf(LayerParse, w.root, "the cyberfile folder has no files")
	}
	return w.itemErrs, nil
}

// folder yields one folder's files, every page of them, then walks its
// subfolders. dir is "" for the root: its name comes with the listing.
func (w *cfWalk) folder(ctx context.Context, node, dir string, depth int) error {
	if depth > cyberfileMaxDepth {
		return Errorf(LayerParse, w.root, "folders nested deeper than %d levels", cyberfileMaxDepth)
	}
	var subs []cfEntry
	for page := 1; page <= cyberfileMaxPages; page++ {
		entries, pages, title, err := w.c.listPage(ctx, node, page)
		if err != nil {
			return err
		}
		if dir == "" {
			dir = sanitizeDirLabel(title, node)
		}
		for _, e := range entries {
			if e.folder {
				subs = append(subs, e)
				continue
			}
			if w.files[e.code] {
				continue
			}
			w.files[e.code] = true
			w.c.mu.Lock()
			w.c.ids[e.code] = e.id // PrepareURL then skips the file's page
			w.c.mu.Unlock()
			name := e.name
			if name == "" {
				name = e.code
			}
			page := w.c.page(e.code)
			it := Item{URL: page, SourcePage: page, Dir: dir, Filename: name, Size: e.size, Index: w.index}
			w.index++
			if err := w.yield(it); err != nil {
				w.stopped = true
				return err
			}
		}
		if page >= pages || len(entries) == 0 {
			break
		}
	}
	for _, s := range subs {
		if w.seen[s.id] {
			continue
		}
		w.seen[s.id] = true
		err := w.folder(ctx, s.id, dir+"/"+sanitizeDirLabel(s.name, s.id), depth+1)
		if err == nil {
			continue
		}
		if w.stopped || ctx.Err() != nil {
			return err
		}
		// One subfolder that can't be listed doesn't sink the rest.
		w.itemErrs = append(w.itemErrs, ItemError{URL: fmt.Sprintf("%s (subfolder %q)", w.root, s.name), Err: err})
	}
	return nil
}
