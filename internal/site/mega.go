package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
)

// MegaName is the registry key.
const MegaName = "mega"

// mega's fundamental difference from the other two sites: files are
// encrypted client-side. The URL isn't enough; the key sits after the # in
// the link, never reaches the server, and the content must be decrypted
// while downloading. That is why this resolver also implements StreamDecoder.
// The crypto side lives in internal/megacrypto.
//
// The API is a single endpoint: https://g.api.mega.co.nz/cs. Commands are
// POSTed as a JSON array and the response is an array too; on error an
// element (or the whole body) is a negative integer.

const (
	ExtraMegaAPI   = "api_endpoint"
	defaultMegaAPI = "https://g.api.mega.co.nz/cs"

	// megaBatch is the number of download URLs requested in a single API
	// call. mega supports batched commands; a 200-file folder is resolved
	// with 4 requests instead of 200.
	megaBatch = 50
)

// NewMega is the factory given to the registry.
func NewMega(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	return &mega{
		cfg:      cfg,
		api:      cfg.ExtraOr(ExtraMegaAPI, defaultMegaAPI),
		nodeKeys: map[string][]byte{},
	}
}

type mega struct {
	cfg SiteConfig
	api string
	seq atomic.Int64

	// nodeKeys holds the packed keys of nodes in folder links (node -> 32
	// bytes). So ResolveOne doesn't have to list the folder from scratch when
	// it must refresh a single folder file on its own.
	mu       sync.Mutex
	nodeKeys map[string][]byte

	// Quota query cache: see quotaWait.
	quotaMu     sync.Mutex
	quotaAt     time.Time
	quotaCached time.Duration
}

// ---------- Link recognition ----------

type megaKind int

const (
	megaFile megaKind = iota
	megaFolder
)

type megaRef struct {
	kind   megaKind
	handle string // file or folder handle
	key    []byte // decoded: file 32 bytes, folder 16 bytes
	node   string // selected node inside a folder (optional)
}

func (m *mega) Match(u string) bool {
	_, err := m.parse(u)
	return err == nil
}

// parse recognizes mega's four link forms:
//
//	https://mega.nz/file/<h>#<k>
//	https://mega.nz/folder/<h>#<k>[/file/<n>|/folder/<n>]
//	https://mega.nz/#!<h>!<k>                  (old)
//	https://mega.nz/#F!<h>!<k>[!<n>|?<n>]       (old folder)
//
// The key length validates the type: file 32, folder 16 bytes. A key of the
// wrong length means the link was copied truncated, and saying so here beats
// saying "meta-MAC mismatch" at the end of the download.
func (m *mega) parse(raw string) (megaRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return megaRef{}, errors.New("empty URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return megaRef{}, fmt.Errorf("could not parse URL: %w", err)
	}
	host := strings.TrimPrefix(normalizeHost(u.Host), "www.")
	known := MatchHost(host, m.cfg.Domains) ||
		MatchHost(host, m.cfg.LegacyDomains) ||
		MatchHost(host, m.cfg.MatchPatterns)
	if !known {
		return megaRef{}, fmt.Errorf("unknown host: %s", host)
	}

	var ref megaRef
	frag := u.Fragment
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch {
	case len(seg) >= 2 && (seg[0] == "file" || seg[0] == "folder"):
		ref.handle = seg[1]
		parts := strings.Split(frag, "/")
		keyStr := parts[0]
		if seg[0] == "file" {
			ref.kind = megaFile
		} else {
			ref.kind = megaFolder
			// #<k>/file/<n> or #<k>/folder/<n>
			if len(parts) >= 3 && (parts[1] == "file" || parts[1] == "folder") {
				ref.node = parts[2]
			}
		}
		ref.key, err = megacrypto.B64Decode(keyStr)
	case strings.HasPrefix(frag, "F!"):
		ref.kind = megaFolder
		rest := strings.TrimPrefix(frag, "F!")
		// selected node with !<n> or ?<n>
		if i := strings.IndexAny(rest, "!?"); i >= 0 {
			ref.handle = rest[:i]
			rest = rest[i+1:]
			if j := strings.IndexAny(rest, "!?"); j >= 0 {
				ref.key, err = megacrypto.B64Decode(rest[:j])
				ref.node = rest[j+1:]
			} else {
				ref.key, err = megacrypto.B64Decode(rest)
			}
		}
	case strings.HasPrefix(frag, "!"):
		ref.kind = megaFile
		rest := strings.TrimPrefix(frag, "!")
		if i := strings.Index(rest, "!"); i >= 0 {
			ref.handle = rest[:i]
			ref.key, err = megacrypto.B64Decode(rest[i+1:])
		}
	default:
		return megaRef{}, errors.New("not a mega link: expected /file/, /folder/ or #!")
	}

	if err != nil {
		return megaRef{}, fmt.Errorf("key is not base64: %w", err)
	}
	if ref.handle == "" {
		return megaRef{}, errors.New("missing handle")
	}
	want := 32
	if ref.kind == megaFolder {
		want = 16
	}
	if len(ref.key) != want {
		return megaRef{}, fmt.Errorf("key is %d bytes, expected %d: the link may be truncated", len(ref.key), want)
	}
	return ref, nil
}

// canonical produces a single-form URL that ResolveOne can parse again.
func (ref megaRef) canonical(node string) string {
	if ref.kind == megaFile {
		return "https://mega.nz/file/" + ref.handle + "#" + megacrypto.B64Encode(ref.key)
	}
	s := "https://mega.nz/folder/" + ref.handle + "#" + megacrypto.B64Encode(ref.key)
	if node != "" {
		s += "/file/" + node
	}
	return s
}

// ---------- API ----------

func (m *mega) client() *http.Client {
	if m.cfg.HTTPClient != nil {
		return m.cfg.HTTPClient
	}
	return http.DefaultClient
}

// megaAPIError turns the API's negative integer errors into layers.
//
// Quota (-17) and block (-16) must be read SEPARATELY from the others: quota
// means "wait a few hours", block means "this file will never come". Passing
// both off as "API error" sends the user to fiddle with the config.
func megaAPIError(code int, evidence string) error {
	switch code {
	case -3, -4, -18:
		return &megaTransient{code: code}
	case -2:
		return Errorf(LayerParse, evidence, "API request rejected (%d): the handle may be invalid", code)
	case -9:
		return Errorf(LayerItemPage, evidence, "file does not exist or was removed (%d)", code)
	case -11:
		return Errorf(LayerItemPage, evidence, "access denied (%d)", code)
	case -14:
		return Errorf(LayerItemPage, evidence, "invalid key (%d): the link may be truncated", code)
	case -16:
		return Errorf(LayerItemPage, evidence, "file blocked (%d): copyright or abuse report", code)
	case -17:
		return &QuotaError{Err: Errorf(LayerCDN, evidence, "mega transfer quota exceeded (%d): per-IP limit", code)}
	default:
		return Errorf(LayerFetch, evidence, "API error %d", code)
	}
}

// megaTransient covers the cases where the API says "try again shortly".
type megaTransient struct{ code int }

func (e *megaTransient) Error() string { return fmt.Sprintf("mega API transient error %d", e.code) }

// call sends the commands in a single request and returns the raw result per command.
//
// On transient errors (-3 EAGAIN, -4 rate limit, -18, 5xx) it retries a few
// times on its own: resolver calls fall outside the download policy, so if
// we don't retry here they are never retried.
func (m *mega) call(ctx context.Context, folder string, cmds []any) ([]json.RawMessage, error) {
	payload, err := json.Marshal(cmds)
	if err != nil {
		return nil, err
	}

	const attempts = 4
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
		}
		out, err := m.callOnce(ctx, folder, payload)
		if err == nil {
			return out, nil
		}
		last = err
		var tr *megaTransient
		if !errors.As(err, &tr) {
			return nil, m.withQuotaWait(ctx, err)
		}
		m.cfg.Logln("mega: %v, retrying (%d/%d)", err, attempt+1, attempts)
	}
	return nil, Errorf(LayerFetch, m.api, "API returned transient errors in a row: %v", last)
}

// ---------- Quota ----------

// quotaProbeTTL: for 509s arriving back to back the API is asked once.
const quotaProbeTTL = time.Minute

// withQuotaWait adds the reset time reported by the site to a quota error.
// ctx may be nil (ClassifyStatus has no context); then a background context
// with a short timeout is used.
func (m *mega) withQuotaWait(ctx context.Context, err error) error {
	q, ok := QuotaOf(err)
	if !ok || q.Wait > 0 {
		return err
	}
	q.Wait = m.quotaWait(ctx)
	return err
}

// quotaWait asks for the reset time with the "uq" (user quota) command.
//
// MEASURED (2026-09-14, with the quota full): the anonymous call
// {"a":"uq","xfer":1} returned: bt=18349 (seconds until reset), tar=0
// (remaining allowance), tah=[0,0,0,0,0,5368709120] (buckets of the last 6
// hours; exactly 5 GiB spent). The quota is per IP, ~5 GiB in a sliding
// window of about 6 hours.
//
// Returns 0 if unknown; the caller uses its own default.
func (m *mega) quotaWait(ctx context.Context) time.Duration {
	m.quotaMu.Lock()
	if time.Since(m.quotaAt) < quotaProbeTTL {
		d := m.quotaCached
		m.quotaMu.Unlock()
		return d
	}
	// The timestamp is set BEFORE the call: if the "uq" call itself returns a
	// quota error, call() comes back here; the timestamp answers it from the
	// cache (0), so there is no infinite loop.
	m.quotaAt, m.quotaCached = time.Now(), 0
	m.quotaMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	var wait time.Duration
	if st, err := m.quotaStatus(ctx); err == nil && st.ResetIn > 0 {
		wait = time.Duration(st.ResetIn) * time.Second
		// Bounds: a nonsense value from the API must not lock the queue for days.
		if wait < time.Minute {
			wait = time.Minute
		}
		if wait > 6*time.Hour {
			wait = 6 * time.Hour
		}
	}
	m.quotaMu.Lock()
	m.quotaCached = wait
	m.quotaMu.Unlock()
	return wait
}

// megaQuota is the useful field of the "uq" response.
//
// MEASURED (2026-09-14): the "tar" field is NOT the "remaining allowance".
// It came back as tar=0 both with the quota full (5 GiB in the last bucket
// of tah) and empty (tah all zero, download succeeding). So "is there
// allowance" is NOT asked of the API; the queue learns it by actually trying
// a waiting file. Only bt is used, and only as an upper bound: the quota
// resets when the bucket that filled it leaves the window, and bt was
// consistent with that (5h 6m, then 5h 14m).
type megaQuota struct {
	ResetIn int64 `json:"bt"` // seconds until the window turns over
}

// quotaStatus asks the API for the quota (10 s timeout).
func (m *mega) quotaStatus(ctx context.Context) (megaQuota, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	results, err := m.call(ctx, "", []any{map[string]any{"a": "uq", "xfer": 1}})
	if err != nil {
		return megaQuota{}, err
	}
	if len(results) == 0 {
		return megaQuota{}, Errorf(LayerParse, m.api, "uq returned an empty array")
	}
	var st megaQuota
	if err := m.decodeResult(results[0], &st, m.api); err != nil {
		return megaQuota{}, err
	}
	return st, nil
}

func (m *mega) callOnce(ctx context.Context, folder string, payload []byte) ([]json.RawMessage, error) {
	endpoint := m.api + "?id=" + strconv.FormatInt(m.seq.Add(1), 10)
	if folder != "" {
		endpoint += "&n=" + url.QueryEscape(folder)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, Errorf(LayerFetch, endpoint, "could not build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", m.cfg.UserAgent)
	}

	resp, err := m.client().Do(req)
	if err != nil {
		return nil, &LayerError{Layer: LayerFetch, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	m.cfg.Recordln("api.json", body)

	if resp.StatusCode >= 500 {
		return nil, &megaTransient{code: -resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Errorf(LayerFetch, endpoint, "API HTTP %s", resp.Status)
	}

	trim := bytes.TrimSpace(body)
	// The whole body may be a single number: [{"a":"g"}] -> -3
	if len(trim) > 0 && trim[0] != '[' {
		code, cerr := strconv.Atoi(string(trim))
		if cerr != nil {
			return nil, Errorf(LayerParse, endpoint, "API response is neither an array nor a number: %.60s", trim)
		}
		return nil, megaAPIError(code, endpoint)
	}
	var results []json.RawMessage
	if err := json.Unmarshal(trim, &results); err != nil {
		return nil, Errorf(LayerParse, endpoint, "API response is not JSON: %v", err)
	}
	return results, nil
}

// decodeResult unpacks a single command result into out; if it is a number
// it turns it into an error.
func (m *mega) decodeResult(raw json.RawMessage, out any, evidence string) error {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		code, err := strconv.Atoi(string(t))
		if err != nil {
			return Errorf(LayerParse, evidence, "could not understand the API result: %.60s", t)
		}
		return m.withQuotaWait(nil, megaAPIError(code, evidence))
	}
	if err := json.Unmarshal(t, out); err != nil {
		return Errorf(LayerParse, evidence, "API result is not JSON: %v", err)
	}
	return nil
}

// megaGetResp is the "g" command's response: size, encrypted attributes, download URL.
type megaGetResp struct {
	Size  int64  `json:"s"`
	Attrs string `json:"at"`
	URL   string `json:"g"`
	Err   int    `json:"e"`
}

// megaNode is a node in a folder listing.
type megaNode struct {
	Handle string `json:"h"`
	Parent string `json:"p"`
	Type   int    `json:"t"` // 0 file, 1 folder
	Attrs  string `json:"a"`
	Key    string `json:"k"` // "<share>:<base64 encrypted key>"
	Size   int64  `json:"s"`
}

// ---------- Resolver ----------

func (m *mega) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := m.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if ref.kind == megaFile {
		it, err := m.resolveFile(ctx, ref)
		if err != nil {
			return nil, err
		}
		return nil, yield(it)
	}
	return m.resolveFolder(ctx, ref, yield)
}

// resolveFile resolves a single file link.
func (m *mega) resolveFile(ctx context.Context, ref megaRef) (Item, error) {
	src := ref.canonical("")
	results, err := m.call(ctx, "", []any{map[string]any{"a": "g", "g": 1, "p": ref.handle}})
	if err != nil {
		return Item{}, err
	}
	if len(results) == 0 {
		return Item{}, Errorf(LayerParse, src, "API returned an empty array")
	}
	var g megaGetResp
	if err := m.decodeResult(results[0], &g, src); err != nil {
		return Item{}, err
	}
	return m.itemFromGet(g, ref.key, src, "", 0)
}

// itemFromGet builds an Item from a "g" response and a packed key.
func (m *mega) itemFromGet(g megaGetResp, packed []byte, src, dir string, index int) (Item, error) {
	if g.Err != 0 {
		return Item{}, megaAPIError(g.Err, src)
	}
	if g.URL == "" {
		return Item{}, Errorf(LayerItemPage, src, "API gave no download URL")
	}
	key, err := megacrypto.UnpackFileKey(packed)
	if err != nil {
		return Item{}, Errorf(LayerParse, src, "%v", err)
	}
	attrs, err := megacrypto.DecryptAttrs(key.AES, g.Attrs)
	if err != nil {
		return Item{}, Errorf(LayerParse, src, "%v", err)
	}
	if attrs.Name == "" {
		return Item{}, Errorf(LayerParse, src, "no file name in the attributes")
	}
	return Item{
		URL:        g.URL,
		SourcePage: src,
		Dir:        dir,
		Filename:   attrs.Name,
		Size:       g.Size,
		Index:      index,
		Secret:     packed,
	}, nil
}

// megaEntry is a single file to download that came out of a folder listing.
type megaEntry struct {
	node   string
	packed []byte
	name   string
	size   int64
	dir    string
}

// listFolder decodes a folder link's nodes and returns the files to download
// together with their folder path. If ref.node is set, only that node (a
// file) or what is under that subfolder.
func (m *mega) listFolder(ctx context.Context, ref megaRef) ([]megaEntry, error) {
	src := ref.canonical("")
	results, err := m.call(ctx, ref.handle, []any{map[string]any{"a": "f", "c": 1, "r": 1}})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, Errorf(LayerParse, src, "API returned an empty array")
	}
	var tree struct {
		Nodes []megaNode `json:"f"`
	}
	if err := m.decodeResult(results[0], &tree, src); err != nil {
		return nil, err
	}
	if len(tree.Nodes) == 0 {
		return nil, Errorf(LayerParse, src, "folder is empty or could not be listed")
	}

	byHandle := map[string]megaNode{}
	for _, n := range tree.Nodes {
		byHandle[n.Handle] = n
	}
	root := megaRootOf(tree.Nodes, byHandle)

	// Folder names: each folder node's attributes are decrypted with a folder
	// key (directly with the 16 bytes, NOT with a derived key as for files).
	// MEASURED: the root included — the share key (the one in the link) is NOT
	// the root's own key; the root's key also arrives wrapped in its "k" field.
	// The link key is only a last-resort candidate (if the root has no "k").
	folderName := map[string]string{}
	for _, n := range tree.Nodes {
		if n.Type != 1 {
			continue
		}
		folderName[n.Handle] = n.Handle
		cands := append(m.nodeKeyCandidates(ref, root, n), ref.key)
		for _, dk := range cands {
			if len(dk) != 16 {
				continue
			}
			if a, aerr := megacrypto.DecryptAttrs(dk, n.Attrs); aerr == nil && a.Name != "" {
				folderName[n.Handle] = a.Name
				break
			}
		}
	}

	// The subfolder path is built as "Root/Sub"; the downloader turns "/"
	// into "-" and downloads into a single folder ("Root-Sub"). The structure
	// information is kept, nested folders are not created. Enough for v1.
	pathOf := func(h string) string {
		var parts []string
		for cur := h; cur != ""; {
			n, ok := byHandle[cur]
			if !ok {
				break
			}
			parts = append([]string{folderName[cur]}, parts...)
			if cur == root {
				break
			}
			cur = n.Parent
		}
		return strings.Join(parts, "/")
	}
	under := func(h string) bool {
		// Is h under ref.node (when ref.node is a subfolder)?
		if ref.node == "" {
			return true
		}
		for cur := h; cur != ""; {
			if cur == ref.node {
				return true
			}
			n, ok := byHandle[cur]
			if !ok || cur == root {
				return false
			}
			cur = n.Parent
		}
		return false
	}

	var out []megaEntry
	for _, n := range tree.Nodes {
		if n.Type != 0 {
			continue
		}
		if ref.node != "" && n.Handle != ref.node && !under(n.Parent) {
			continue
		}
		packed, name, err := m.fileKeyOf(ref, root, n)
		if err != nil {
			m.cfg.Logln("mega: node %s: %v", n.Handle, err)
			continue
		}
		out = append(out, megaEntry{
			node: n.Handle, packed: packed, name: name, size: n.Size, dir: pathOf(n.Parent),
		})
	}

	m.mu.Lock()
	for _, e := range out {
		m.nodeKeys[e.node] = e.packed
	}
	m.mu.Unlock()
	return out, nil
}

// megaRootOf finds the root node of a folder link.
//
// MEASURED: the handle in the link (mega.nz/folder/<handle>) is the SHARE's
// handle, NOT the root folder's node handle; the two are never equal. The
// labels in the "k" field and the "p" chain work with node handles, so the
// root is derived from the tree: the folder whose parent isn't in the list.
// The API returns the root first; on ambiguity that one is taken.
func megaRootOf(nodes []megaNode, byHandle map[string]megaNode) string {
	for _, n := range nodes {
		if n.Type == 1 {
			if _, ok := byHandle[n.Parent]; !ok {
				return n.Handle
			}
		}
	}
	return nodes[0].Handle
}

// nodeKeyCandidates opens the keys in the node's "k" field with the folder
// key and returns them in order of preference.
//
// The field has the form "<share>:<key>", "/"-separated if there are several.
// MEASURED: if the owner also shared the folder from higher up, the first
// label belongs to that upper share and we don't have its key; a key
// "opened" with it silently comes out as garbage. That is why the candidate
// labeled with the root is moved to the front, and the caller verifies the
// candidates by decrypting the attributes — whichever yields the "MEGA"
// prefix is the right key.
func (m *mega) nodeKeyCandidates(ref megaRef, root string, n megaNode) [][]byte {
	var preferred, others [][]byte
	for _, part := range strings.Split(n.Key, "/") {
		i := strings.Index(part, ":")
		if i < 0 {
			continue
		}
		enc, err := megacrypto.B64Decode(part[i+1:])
		if err != nil {
			continue
		}
		dec, err := megacrypto.DecryptNodeKey(ref.key, enc)
		if err != nil {
			continue
		}
		if part[:i] == root {
			preferred = append(preferred, dec)
		} else {
			others = append(others, dec)
		}
	}
	return append(preferred, others...)
}

// fileKeyOf returns a file node's packed key and name; it picks the candidate
// that can decrypt the attributes.
func (m *mega) fileKeyOf(ref megaRef, root string, n megaNode) (packed []byte, name string, err error) {
	if n.Key == "" {
		return nil, "", errors.New("k field is empty")
	}
	cands := m.nodeKeyCandidates(ref, root, n)
	if len(cands) == 0 {
		return nil, "", errors.New("no key in the k field")
	}
	var last error
	for _, c := range cands {
		key, uerr := megacrypto.UnpackFileKey(c)
		if uerr != nil {
			last = uerr
			continue
		}
		attrs, aerr := megacrypto.DecryptAttrs(key.AES, n.Attrs)
		if aerr != nil {
			last = aerr
			continue
		}
		if attrs.Name == "" {
			last = errors.New("no file name in the attributes")
			continue
		}
		return c, attrs.Name, nil
	}
	return nil, "", fmt.Errorf("none of the %d key candidates decrypted the attributes: %v", len(cands), last)
}

// resolveFolder resolves the folder's files with batched "g" calls and yields them.
func (m *mega) resolveFolder(ctx context.Context, ref megaRef, yield func(Item) error) ([]ItemError, error) {
	entries, err := m.listFolder(ctx, ref)
	if err != nil {
		return nil, err
	}
	if ref.node != "" && len(entries) == 0 {
		return nil, Errorf(LayerItemPage, ref.canonical(ref.node), "the selected node was not found in the folder")
	}

	var itemErrs []ItemError
	index := 0
	for start := 0; start < len(entries); start += megaBatch {
		end := start + megaBatch
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[start:end]
		cmds := make([]any, 0, len(batch))
		for _, e := range batch {
			cmds = append(cmds, map[string]any{"a": "g", "g": 1, "n": e.node})
		}
		results, err := m.call(ctx, ref.handle, cmds)
		if err != nil {
			return itemErrs, err
		}
		if len(results) != len(batch) {
			return itemErrs, Errorf(LayerParse, ref.canonical(""),
				"API returned %d results, expected %d", len(results), len(batch))
		}
		for i, e := range batch {
			src := ref.canonical(e.node)
			var g megaGetResp
			if derr := m.decodeResult(results[i], &g, src); derr != nil {
				itemErrs = append(itemErrs, ItemError{URL: src, Err: derr})
				continue
			}
			it, ierr := m.itemFromGet(g, e.packed, src, e.dir, index)
			if ierr != nil {
				itemErrs = append(itemErrs, ItemError{URL: src, Err: ierr})
				continue
			}
			// The name and size from the listing are more reliable: "g"
			// sometimes carries no attributes.
			if it.Filename == "" {
				it.Filename = e.name
			}
			if it.Size <= 0 {
				it.Size = e.size
			}
			index++
			if yerr := yield(it); yerr != nil {
				return itemErrs, yerr
			}
		}
	}
	return itemErrs, nil
}

// ResolveOne refreshes an expired download URL.
func (m *mega) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := m.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind == megaFile {
		return m.resolveFile(ctx, ref)
	}
	if ref.node == "" {
		return Item{}, Errorf(LayerParse, sourcePage, "a folder link can't be resolved as a single item; /file/<node> is required")
	}

	m.mu.Lock()
	packed, ok := m.nodeKeys[ref.node]
	m.mu.Unlock()
	if !ok {
		// Not in the cache (e.g. the process restarted): list the folder from scratch.
		if _, lerr := m.listFolder(ctx, megaRef{kind: megaFolder, handle: ref.handle, key: ref.key}); lerr != nil {
			return Item{}, lerr
		}
		m.mu.Lock()
		packed, ok = m.nodeKeys[ref.node]
		m.mu.Unlock()
		if !ok {
			return Item{}, Errorf(LayerItemPage, sourcePage, "node not found in the folder")
		}
	}

	results, err := m.call(ctx, ref.handle, []any{map[string]any{"a": "g", "g": 1, "n": ref.node}})
	if err != nil {
		return Item{}, err
	}
	if len(results) == 0 {
		return Item{}, Errorf(LayerParse, sourcePage, "API returned an empty array")
	}
	var g megaGetResp
	if err := m.decodeResult(results[0], &g, sourcePage); err != nil {
		return Item{}, err
	}
	return m.itemFromGet(g, packed, sourcePage, "", 0)
}

// ---------- Downloader hooks ----------

// DecodeStream implements site.StreamDecoder. The key travels in Item.Secret.
func (m *mega) DecodeStream(it Item, offset int64, saved []byte, r io.Reader) (DecodedStream, error) {
	key, err := megacrypto.UnpackFileKey(it.Secret)
	if err != nil {
		return nil, fmt.Errorf("mega: item key missing or corrupt: %w", err)
	}
	return megacrypto.NewStream(key, offset, saved, r)
}

// DecodeRange implements site.RangeDecoder: AES-CTR decrypts any range on its
// own, so a file can be fetched over several connections.
func (m *mega) DecodeRange(it Item, offset int64, r io.Reader) (io.Reader, error) {
	key, err := megacrypto.UnpackFileKey(it.Secret)
	if err != nil {
		return nil, fmt.Errorf("mega: item key missing or corrupt: %w", err)
	}
	return megacrypto.NewRangeReader(key, offset, r)
}

// RangeURL implements site.RangeURLer: mega's storage servers take the range
// in the path, ".../<start>-<end>" with an inclusive end, the form mega's own
// clients, MegaBasterd (ChunkWriterManager.genChunkUrl) and go-mega use.
func (m *mega) RangeURL(rawURL string, start, end int64) string {
	suffix := "/" + strconv.FormatInt(start, 10)
	if end > start {
		suffix += "-" + strconv.FormatInt(end-1, 10)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL + suffix
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + suffix
	u.RawPath = ""
	return u.String()
}

// NewVerifier implements site.RangeDecoder: the meta-MAC over the finished file.
func (m *mega) NewVerifier(it Item) (Verifier, error) {
	key, err := megacrypto.UnpackFileKey(it.Secret)
	if err != nil {
		return nil, fmt.Errorf("mega: item key missing or corrupt: %w", err)
	}
	return megacrypto.NewVerifier(key)
}

// ClassifyStatus implements site.StatusClassifier.
//
// 509 is mega's per-IP transfer quota. By the generic rule it would count as
// "5xx, transient" and be retried over and over within minutes; but the quota
// stays full for hours. It is reported as a QuotaError: the retry policy
// stops, the queue moves the job to "waiting for quota" and at the reset time
// (or when the user changes IP and presses ▶) resolves and tries again.
//
// 403 returns nil: the downloader treats it as "URL expired" and refreshes it
// with ResolveOne. MEASURED: the "ip" field in the "g" response BINDS the
// download URL to the requesting IP; when the VPN changes, the old URL gives
// 403 and a fresh "g" is needed.
func (m *mega) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode == 509 {
		evidence := ""
		if resp.Request != nil && resp.Request.URL != nil {
			evidence = resp.Request.URL.Host
		}
		return m.withQuotaWait(nil, &QuotaError{Err: Errorf(LayerCDN, evidence,
			"mega transfer quota exceeded (HTTP 509): per-IP limit")})
	}
	return nil
}

// ---------- Diagnosis ----------

// Diagnose deliberately sends an invalid command to the API. The expected
// answer is a negative number: it means "I reached the API, it speaks JSON
// and answered me". No real file handle is needed.
func (m *mega) Diagnose(ctx context.Context) ([]LayerResult, error) {
	canaries := m.cfg.CanaryURLs
	if len(canaries) == 0 {
		canaries = []string{m.api}
	}
	var last []LayerResult
	for _, c := range canaries {
		res := m.diagnoseOne(ctx, c)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (m *mega) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	var out []LayerResult
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

	probe := &mega{cfg: m.cfg, api: canary, nodeKeys: map[string][]byte{}}
	_, perr := probe.callOnce(ctx, "", []byte(`[{"a":"g","p":"AAAAAAAA"}]`))
	layer, _ := LayerOf(perr)
	switch {
	case perr == nil:
		// Even an invalid handle got a result array back; the API is talking.
		fallthrough
	case layer == LayerParse || layer == LayerItemPage:
		// A negative number came back (-2/-9): exactly what we expect.
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "no challenge"},
			LayerResult{Layer: LayerFetch, Status: StatusOK, Detail: "API answered"},
			LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "JSON decoded, error code in the expected form"},
			LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "mega has no separate item page, the chain is just the API"},
			LayerResult{Layer: LayerCDN, Status: StatusOK, Detail: "download URLs come from the API; the quota only shows while downloading"},
		)
	case layer == LayerTLS:
		out = append(out, LayerResult{Layer: LayerTLS, Status: StatusFail,
			Detail: "handshake failed", Evidence: collapseSpace(perr.Error())})
	default:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "handshake OK"},
			LayerResult{Layer: LayerFetch, Status: StatusFail,
				Detail: "could not reach the API", Evidence: collapseSpace(perr.Error())})
	}
	return out
}
