package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
)

// --- Fake mega API ---
//
// It uses the real crypto: attributes really are encrypted, node keys really
// are wrapped with the folder key. So what the resolver decodes is the result
// of the crypto, not what the fake server says is "right".

type megaFakeFile struct {
	packed []byte // 32-byte packed key
	name   string
	size   int64
}

type megaFakeNode struct {
	handle, parent, name string
	isFolder             bool
	packed               []byte // file: 32, folder: 16
	size                 int64
}

type megaFakeFolder struct {
	key   []byte // 16
	nodes []megaFakeNode
	// root is the root folder's NODE handle. In the real API it is ALWAYS
	// different from the (share) handle in the link; the "k" labels and the
	// "p" chain work with it. If left empty the first node counts as the root.
	root string
	// foreign reproduces the case where the owner also shared the same tree
	// from higher up: each node's "k" field first has this share's entry
	// (whose key we don't have), then ours. A live observation.
	foreign []byte
}

func (fo megaFakeFolder) rootHandle() string {
	if fo.root != "" {
		return fo.root
	}
	return fo.nodes[0].handle
}

type megaFakeAPI struct {
	t       *testing.T
	files   map[string]megaFakeFile
	folders map[string]megaFakeFolder
	cdn     string

	mu       sync.Mutex
	calls    []string // "g:p=<h>", "g:n=<h>", "f", "uq"
	bareErrs []int    // error codes to return as the whole body on the next calls
	nodeErr  map[string]int
	// If quotaResetSec > 0 the "uq" command returns {"bt": quotaResetSec,
	// "tar": 0} (the shape the live API gives with the quota full); if 0, -2.
	quotaResetSec int64
}

func newMegaFakeAPI(t *testing.T) *megaFakeAPI {
	return &megaFakeAPI{
		t: t, files: map[string]megaFakeFile{}, folders: map[string]megaFakeFolder{},
		cdn: "https://cdn.test", nodeErr: map[string]int{},
	}
}

func (f *megaFakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("mega API method = %s", r.Method)
	}
	body, _ := io.ReadAll(r.Body)
	var cmds []map[string]any
	if err := json.Unmarshal(body, &cmds); err != nil {
		f.t.Errorf("API body is not a JSON array: %v", err)
		http.Error(w, "bad", 400)
		return
	}
	folder := r.URL.Query().Get("n")

	f.mu.Lock()
	if len(f.bareErrs) > 0 {
		code := f.bareErrs[0]
		f.bareErrs = f.bareErrs[1:]
		f.mu.Unlock()
		fmt.Fprint(w, code)
		return
	}
	f.mu.Unlock()

	var results []any
	for _, c := range cmds {
		switch c["a"] {
		case "g":
			if p, ok := c["p"].(string); ok {
				f.record("g:p=" + p)
				file, ok := f.files[p]
				if !ok {
					results = append(results, -9)
					continue
				}
				results = append(results, f.getResp(file.packed, file.name, file.size, p))
			} else if n, ok := c["n"].(string); ok {
				f.record("g:n=" + n)
				if code, bad := f.nodeErr[n]; bad {
					results = append(results, code)
					continue
				}
				node := f.findNode(folder, n)
				if node == nil {
					results = append(results, -9)
					continue
				}
				results = append(results, f.getResp(node.packed, node.name, node.size, n))
			}
		case "f":
			f.record("f")
			fo, ok := f.folders[folder]
			if !ok {
				results = append(results, -9)
				continue
			}
			var nodes []map[string]any
			for _, n := range fo.nodes {
				aesKey := n.packed
				if !n.isFolder {
					k, _ := megacrypto.UnpackFileKey(n.packed)
					aesKey = k.AES
				}
				at, _ := megacrypto.EncryptAttrs(aesKey, megacrypto.Attrs{Name: n.name})
				enc, _ := megacrypto.EncryptNodeKey(fo.key, n.packed)
				typ := 0
				if n.isFolder {
					typ = 1
				}
				// Real API: label = the root NODE handle (not the link handle);
				// the root node carries its own "k" too.
				k := fo.rootHandle() + ":" + megacrypto.B64Encode(enc)
				if fo.foreign != nil {
					fenc, _ := megacrypto.EncryptNodeKey(fo.foreign, n.packed)
					k = "OwNeRdIr:" + megacrypto.B64Encode(fenc) + "/" + k
				}
				m := map[string]any{"h": n.handle, "p": n.parent, "t": typ, "a": at, "k": k}
				if !n.isFolder {
					m["s"] = n.size
				}
				nodes = append(nodes, m)
			}
			results = append(results, map[string]any{"f": nodes})
		case "uq":
			f.record("uq")
			if f.quotaResetSec > 0 {
				results = append(results, map[string]any{"bt": f.quotaResetSec, "tar": 0})
			} else {
				results = append(results, -2)
			}
		default:
			results = append(results, -2)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}

func (f *megaFakeAPI) getResp(packed []byte, name string, size int64, handle string) map[string]any {
	k, _ := megacrypto.UnpackFileKey(packed)
	at, _ := megacrypto.EncryptAttrs(k.AES, megacrypto.Attrs{Name: name})
	return map[string]any{"s": size, "at": at, "g": f.cdn + "/dl/" + handle}
}

func (f *megaFakeAPI) findNode(folder, handle string) *megaFakeNode {
	fo, ok := f.folders[folder]
	if !ok {
		return nil
	}
	for i := range fo.nodes {
		if fo.nodes[i].handle == handle {
			return &fo.nodes[i]
		}
	}
	return nil
}

func (f *megaFakeAPI) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *megaFakeAPI) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func megaCfg() SiteConfig {
	return SiteConfig{
		Name:          MegaName,
		Domains:       []string{"mega.nz", "mega.co.nz"},
		RefererPolicy: RefererNone,
		CanaryURLs:    []string{"https://api.test/cs"},
		Extra:         map[string]string{ExtraMegaAPI: "https://api.test/cs"},
	}.WithDefaults()
}

func newMegaWith(t *testing.T, api *megaFakeAPI) *mega {
	t.Helper()
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{"api.test": api.handler}}
	cfg := megaCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return NewMega(cfg).(*mega)
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// packedKeyFor produces a packed key matching the plaintext (with the right
// meta-MAC): the exact equivalent of the 32 bytes that arrive in a link.
func packedKeyFor(t *testing.T, plain []byte) []byte {
	t.Helper()
	aesKey, nonce := randBytes(t, 16), randBytes(t, 8)
	mac, err := megacrypto.MetaMACOf(aesKey, nonce, plain)
	if err != nil {
		t.Fatal(err)
	}
	return megacrypto.PackFileKey(aesKey, nonce, mac)
}

// --- Link recognition ---

func TestMegaParseForms(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	fileKey := megacrypto.B64Encode(make([]byte, 32))
	folderKey := megacrypto.B64Encode(make([]byte, 16))

	cases := []struct {
		in           string
		kind         megaKind
		handle, node string
	}{
		{"https://mega.nz/file/AbCdEfGh#" + fileKey, megaFile, "AbCdEfGh", ""},
		{"https://mega.co.nz/file/AbCdEfGh#" + fileKey, megaFile, "AbCdEfGh", ""},
		{"https://www.mega.nz/file/AbCdEfGh#" + fileKey, megaFile, "AbCdEfGh", ""},
		{"mega.nz/file/AbCdEfGh#" + fileKey, megaFile, "AbCdEfGh", ""},
		{"https://mega.nz/folder/FoLdErHa#" + folderKey, megaFolder, "FoLdErHa", ""},
		{"https://mega.nz/folder/FoLdErHa#" + folderKey + "/file/NoDeHaNd", megaFolder, "FoLdErHa", "NoDeHaNd"},
		{"https://mega.nz/folder/FoLdErHa#" + folderKey + "/folder/SuBfOlDr", megaFolder, "FoLdErHa", "SuBfOlDr"},
		{"https://mega.nz/#!AbCdEfGh!" + fileKey, megaFile, "AbCdEfGh", ""},
		{"https://mega.nz/#F!FoLdErHa!" + folderKey, megaFolder, "FoLdErHa", ""},
		{"https://mega.nz/#F!FoLdErHa!" + folderKey + "!NoDeHaNd", megaFolder, "FoLdErHa", "NoDeHaNd"},
		{"https://mega.nz/#F!FoLdErHa!" + folderKey + "?NoDeHaNd", megaFolder, "FoLdErHa", "NoDeHaNd"},
	}
	for _, c := range cases {
		ref, err := m.parse(c.in)
		if err != nil {
			t.Errorf("parse(%q): %v", c.in, err)
			continue
		}
		if ref.kind != c.kind || ref.handle != c.handle || ref.node != c.node {
			t.Errorf("parse(%q) = kind=%d handle=%q node=%q", c.in, ref.kind, ref.handle, ref.node)
		}
	}

	bad := []string{
		"https://pixeldrain.com/l/abc",
		"https://mega.nz/",
		"https://mega.nz/file/AbCdEfGh",                             // no key
		"https://mega.nz/file/AbCdEfGh#" + folderKey,                // 16 bytes for a file
		"https://mega.nz/folder/FoLdErHa#" + fileKey,                // 32 bytes for a folder
		"https://mega.nz/file/AbCdEfGh#" + fileKey[:len(fileKey)-5], // truncated
		"https://mega.attacker.com/file/AbCdEfGh#" + fileKey,
	}
	for _, in := range bad {
		if m.Match(in) {
			t.Errorf("Match(%q) = true, want false", in)
		}
	}
}

// --- Single file ---

func TestMegaResolveFileAndDecode(t *testing.T) {
	plain := randBytes(t, 200*1024+9)
	packed := packedKeyFor(t, plain)
	api := newMegaFakeAPI(t)
	api.files["FiLeHaNd"] = megaFakeFile{packed: packed, name: "Holiday — Zoë.mp4", size: int64(len(plain))}
	m := newMegaWith(t, api)

	link := "https://mega.nz/file/FiLeHaNd#" + megacrypto.B64Encode(packed)
	var items []Item
	itemErrs, err := m.Resolve(context.Background(), link, func(it Item) error { items = append(items, it); return nil })
	if err != nil || len(itemErrs) != 0 {
		t.Fatalf("Resolve: %v / %v", err, itemErrs)
	}
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	it := items[0]
	if it.Filename != "Holiday — Zoë.mp4" || it.Size != int64(len(plain)) {
		t.Errorf("item = %+v", it)
	}
	if it.URL != "https://cdn.test/dl/FiLeHaNd" {
		t.Errorf("URL = %q", it.URL)
	}
	if it.SourcePage != link {
		t.Errorf("SourcePage = %q, expected the canonical link", it.SourcePage)
	}
	if !bytes.Equal(it.Secret, packed) {
		t.Error("Secret doesn't carry the packed key")
	}

	// Decoder: given the encrypted body, plaintext must come out and Verify must pass.
	key, _ := megacrypto.UnpackFileKey(packed)
	enc, _ := megacrypto.EncryptCTR(key.AES, key.Nonce, plain)
	ds, err := m.DecodeStream(it, 0, nil, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	got, _ := io.ReadAll(ds)
	if !bytes.Equal(got, plain) {
		t.Fatal("decoded content is not the plaintext")
	}
	if err := ds.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestMegaDecodeStreamRejectsMissingSecret(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	if _, err := m.DecodeStream(Item{}, 0, nil, bytes.NewReader(nil)); err == nil {
		t.Fatal("a decoder was built for an item without a Secret")
	}
}

// --- Folder ---

func setupFolder(t *testing.T, api *megaFakeAPI) (folderKey []byte, packedA, packedB, packedC []byte) {
	t.Helper()
	folderKey = randBytes(t, 16)
	rootKey := randBytes(t, 16) // share key != the root's own key (live observation)
	subKey := randBytes(t, 16)
	packedA = packedKeyFor(t, []byte("a"))
	packedB = packedKeyFor(t, []byte("b"))
	packedC = packedKeyFor(t, []byte("c"))
	// Link handle "FoLdErHa", root NODE "RoOtNoDe", the root's parent
	// "OwNeRdIr" (in the owner's account, not in the list) — the live API's shape.
	api.folders["FoLdErHa"] = megaFakeFolder{key: folderKey, root: "RoOtNoDe", nodes: []megaFakeNode{
		{handle: "RoOtNoDe", parent: "OwNeRdIr", name: "Root Folder", isFolder: true, packed: rootKey},
		{handle: "SuBfOlDr", parent: "RoOtNoDe", name: "Sub", isFolder: true, packed: subKey},
		{handle: "NoDeAAAA", parent: "RoOtNoDe", name: "a.mp4", packed: packedA, size: 100},
		{handle: "NoDeBBBB", parent: "RoOtNoDe", name: "b.mp4", packed: packedB, size: 200},
		{handle: "NoDeCCCC", parent: "SuBfOlDr", name: "c.mp4", packed: packedC, size: 300},
	}}
	return
}

// MEASURED (mega.nz/folder/VVplxTBY): if the owner also shared the tree from
// higher up, each node's "k" field starts with that share's entry. The old
// code said "the one matching the link handle, otherwise the FIRST"; since the
// link handle is never a node handle it always picked the foreign key and all
// 373 files were skipped as "name could not be decrypted". The queue stayed empty.
func TestMegaFolderPicksOwnShareKeyNotForeign(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, _, _ := setupFolder(t, api)
	fo := api.folders["FoLdErHa"]
	fo.foreign = randBytes(t, 16)
	api.folders["FoLdErHa"] = fo
	m := newMegaWith(t, api)

	var got []Item
	itemErrs, err := m.Resolve(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(folderKey),
		func(it Item) error { got = append(got, it); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(itemErrs) != 0 {
		t.Fatalf("item errors: %v", itemErrs)
	}
	if len(got) != 3 {
		t.Fatalf("%d items resolved, want 3", len(got))
	}
	byName := map[string]Item{}
	for _, it := range got {
		byName[it.Filename] = it
	}
	// Folder names must be decrypted with the right key too; the root with the link key.
	if d := byName["c.mp4"].Dir; d != "Root Folder/Sub" {
		t.Errorf("c.mp4 directory = %q", d)
	}
	if d := byName["a.mp4"].Dir; d != "Root Folder" {
		t.Errorf("a.mp4 directory = %q", d)
	}
}

func TestMegaResolveFolder(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, packedA, _, packedC := setupFolder(t, api)
	m := newMegaWith(t, api)

	link := "https://mega.nz/folder/FoLdErHa#" + megacrypto.B64Encode(folderKey)
	var items []Item
	itemErrs, err := m.Resolve(context.Background(), link, func(it Item) error { items = append(items, it); return nil })
	if err != nil || len(itemErrs) != 0 {
		t.Fatalf("Resolve: %v / %v", err, itemErrs)
	}
	if len(items) != 3 {
		t.Fatalf("%d items, want 3", len(items))
	}
	byName := map[string]Item{}
	for _, it := range items {
		byName[it.Filename] = it
	}
	if a := byName["a.mp4"]; a.Dir != "Root Folder" || a.Size != 100 || !bytes.Equal(a.Secret, packedA) {
		t.Errorf("a = %+v", a)
	}
	// A file in the subfolder: path "Root/Sub" (the downloader flattens it with "-").
	if c := byName["c.mp4"]; c.Dir != "Root Folder/Sub" || !bytes.Equal(c.Secret, packedC) {
		t.Errorf("c = %+v", c)
	}
	if byName["a.mp4"].SourcePage != link+"/file/NoDeAAAA" {
		t.Errorf("SourcePage = %q", byName["a.mp4"].SourcePage)
	}
	// Indices must be sequential (the name collision map depends on it).
	seen := map[int]bool{}
	for _, it := range items {
		seen[it.Index] = true
	}
	if !seen[0] || !seen[1] || !seen[2] {
		t.Errorf("indices must be 0,1,2: %v", seen)
	}
	// The folder listing must be 1 call, the download URLs 1 BATCHED call.
	if api.count("f") != 1 || api.count("g:n=") != 3 {
		t.Errorf("calls: %v", api.calls)
	}
}

func TestMegaResolveFolderSelectedFile(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, packedB, _ := setupFolder(t, api)
	m := newMegaWith(t, api)

	link := "https://mega.nz/folder/FoLdErHa#" + megacrypto.B64Encode(folderKey) + "/file/NoDeBBBB"
	var items []Item
	if _, err := m.Resolve(context.Background(), link, func(it Item) error { items = append(items, it); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Filename != "b.mp4" || !bytes.Equal(items[0].Secret, packedB) {
		t.Fatalf("items = %+v", items)
	}
}

func TestMegaResolveFolderSelectedSubfolder(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, _, _ := setupFolder(t, api)
	m := newMegaWith(t, api)

	link := "https://mega.nz/folder/FoLdErHa#" + megacrypto.B64Encode(folderKey) + "/folder/SuBfOlDr"
	var items []Item
	if _, err := m.Resolve(context.Background(), link, func(it Item) error { items = append(items, it); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Filename != "c.mp4" {
		t.Fatalf("wrong subfolder selection: %+v", items)
	}
}

// ResolveOne must work even on a resolver with an empty cache (the process
// restarted): it lists the folder and finds the node.
func TestMegaResolveOneFolderFileColdCache(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, packedB, _ := setupFolder(t, api)
	m := newMegaWith(t, api)

	src := "https://mega.nz/folder/FoLdErHa#" + megacrypto.B64Encode(folderKey) + "/file/NoDeBBBB"
	it, err := m.ResolveOne(context.Background(), src)
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if it.Filename != "b.mp4" || !bytes.Equal(it.Secret, packedB) || it.URL == "" {
		t.Fatalf("item = %+v", it)
	}
	if api.count("f") != 1 {
		t.Errorf("with a cold cache the folder should have been listed once: %v", api.calls)
	}
	// The second call comes from the cache: NO re-listing.
	if _, err := m.ResolveOne(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if api.count("f") != 1 {
		t.Errorf("the folder was listed again with a warm cache: %v", api.calls)
	}
}

func TestMegaResolveOneRejectsBareFolder(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	_, err := m.ResolveOne(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(make([]byte, 16)))
	if err == nil {
		t.Fatal("a folder link without a node was resolved as a single item")
	}
}

// 120 files: download URLs must be fetched in batches of 50 in 3 calls.
func TestMegaFolderBatchesGetCalls(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey := randBytes(t, 16)
	nodes := []megaFakeNode{{handle: "RoOtNoDe", parent: "OwNeRdIr", name: "Root", isFolder: true, packed: folderKey}}
	for i := 0; i < 120; i++ {
		nodes = append(nodes, megaFakeNode{
			handle: fmt.Sprintf("NoDe%04d", i), parent: "RoOtNoDe",
			name: fmt.Sprintf("d%d.bin", i), packed: packedKeyFor(t, []byte{byte(i)}), size: 1,
		})
	}
	api.folders["FoLdErHa"] = megaFakeFolder{key: folderKey, nodes: nodes}
	m := newMegaWith(t, api)

	n := 0
	if _, err := m.Resolve(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(folderKey),
		func(Item) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 120 {
		t.Fatalf("%d items, want 120", n)
	}
	// Number of POSTs via hostRouter.seen: 1 listing + 3 batched g.
	posts := 0
	rt := m.cfg.HTTPClient.Transport.(*hostRouter)
	for _, s := range rt.seen {
		if strings.HasPrefix(s, "api.test") {
			posts++
		}
	}
	if posts != 4 {
		t.Errorf("%d API requests, want 4 (1 list + 3 batched)", posts)
	}
}

// --- Error map ---

func TestMegaAPIErrorMapping(t *testing.T) {
	cases := []struct {
		code  int
		layer Layer
	}{
		{-9, LayerItemPage},
		{-11, LayerItemPage},
		{-16, LayerItemPage},
		{-17, LayerCDN},
		{-2, LayerParse},
		{-99, LayerFetch},
	}
	for _, c := range cases {
		l, ok := LayerOf(megaAPIError(c.code, "x"))
		if !ok || l != c.layer {
			t.Errorf("code %d -> %v, want %v", c.code, l, c.layer)
		}
	}
}

func TestMegaFileNotFound(t *testing.T) {
	api := newMegaFakeAPI(t)
	m := newMegaWith(t, api)
	_, err := m.Resolve(context.Background(), "https://mega.nz/file/NoSuChFi#"+megacrypto.B64Encode(make([]byte, 32)),
		func(Item) error { return nil })
	if l, ok := LayerOf(err); !ok || l != LayerItemPage {
		t.Fatalf("%v for a nonexistent file, expected ItemPage", err)
	}
}

// A transient error (-3) must be retried a few times; success on the second attempt.
func TestMegaTransientErrorIsRetried(t *testing.T) {
	plain := []byte("data")
	packed := packedKeyFor(t, plain)
	api := newMegaFakeAPI(t)
	api.files["FiLeHaNd"] = megaFakeFile{packed: packed, name: "x.bin", size: 4}
	api.bareErrs = []int{-3}
	m := newMegaWith(t, api)

	n := 0
	_, err := m.Resolve(context.Background(), "https://mega.nz/file/FiLeHaNd#"+megacrypto.B64Encode(packed),
		func(Item) error { n++; return nil })
	if err != nil || n != 1 {
		t.Fatalf("expected success after a transient error: %v (n=%d)", err, n)
	}
}

// Quota (-17) is NOT TRANSIENT: it must not be retried; it must be reported
// right away and clearly.
func TestMegaQuotaIsNotRetried(t *testing.T) {
	api := newMegaFakeAPI(t)
	api.bareErrs = []int{-17, -17, -17, -17}
	m := newMegaWith(t, api)

	_, err := m.Resolve(context.Background(), "https://mega.nz/file/FiLeHaNd#"+megacrypto.B64Encode(make([]byte, 32)),
		func(Item) error { return nil })
	if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Fatalf("%v for quota, expected the CDN layer", err)
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Errorf("the error message doesn't mention the quota: %v", err)
	}
	if _, ok := QuotaOf(err); !ok {
		t.Errorf("the quota error is not a QuotaError: %T %v", err, err)
	}
	// Two calls: "g" (-17) and "uq" for the reset time (which also got -17
	// and, thanks to the re-entry guard, didn't spawn a third call).
	if len(api.bareErrs) != 2 {
		t.Errorf("the quota error was retried: %d left", len(api.bareErrs))
	}
}

// The quota error must carry the reset time reported by the API: the queue
// waits accordingly and the user sees "5h 6m" instead of "a few hours".
func TestMegaQuotaCarriesResetTime(t *testing.T) {
	api := newMegaFakeAPI(t)
	api.bareErrs = []int{-17}
	api.quotaResetSec = 18349 // live measurement
	m := newMegaWith(t, api)

	_, err := m.Resolve(context.Background(), "https://mega.nz/file/FiLeHaNd#"+megacrypto.B64Encode(make([]byte, 32)),
		func(Item) error { return nil })
	q, ok := QuotaOf(err)
	if !ok {
		t.Fatalf("expected QuotaError: %v", err)
	}
	if q.Wait != 18349*time.Second {
		t.Errorf("Wait = %s, want 18349s", q.Wait)
	}
	if !strings.Contains(err.Error(), "5h 6m") {
		t.Errorf("no duration in the message: %v", err)
	}
	if api.count("uq") != 1 {
		t.Errorf("uq asked %d times, want 1", api.count("uq"))
	}

	// A 509 must go the same way and must not ask the API again within a minute.
	resp := &http.Response{StatusCode: 509, Request: &http.Request{URL: mustURL("http://gfs1.userstorage.mega.co.nz/dl/x")}}
	cerr := m.ClassifyStatus(resp, nil)
	q2, ok := QuotaOf(cerr)
	if !ok || q2.Wait != 18349*time.Second {
		t.Errorf("wrong QuotaError/Wait for 509: %v", cerr)
	}
	if api.count("uq") != 1 {
		t.Errorf("uq was asked again after the 509: %d", api.count("uq"))
	}
}

// If a single node in a folder errors, the album must NOT fail; ItemErrors must be collected.
func TestMegaFolderCollectsPerItemErrors(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, _, _ := setupFolder(t, api)
	api.nodeErr["NoDeBBBB"] = -16
	m := newMegaWith(t, api)

	n := 0
	itemErrs, err := m.Resolve(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(folderKey),
		func(Item) error { n++; return nil })
	if err != nil {
		t.Fatalf("the folder must not fail: %v", err)
	}
	if n != 2 || len(itemErrs) != 1 {
		t.Fatalf("n=%d itemErrs=%d; want 2 and 1", n, len(itemErrs))
	}
	if l, _ := LayerOf(itemErrs[0].Err); l != LayerItemPage {
		t.Errorf("blocked file layer = %v", l)
	}
}

// --- Quota (HTTP 509) ---

func TestMegaClassifyStatus509(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	resp := &http.Response{StatusCode: 509}
	err := m.ClassifyStatus(resp, nil)
	if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Fatalf("509 -> %v, expected CDN", err)
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("the quota error was marked retryable")
	}
	// 403 must return nil: the downloader treats it as "URL expired" and refreshes it.
	if err := m.ClassifyStatus(&http.Response{StatusCode: 403}, nil); err != nil {
		t.Errorf("expected nil for 403: %v", err)
	}
}

// --- Diagnosis ---

func TestMegaDiagnoseHealthy(t *testing.T) {
	api := newMegaFakeAPI(t)
	// Diagnose does a real DNS lookup; the only fake host that resolves is localhost.
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{"localhost": api.handler}}
	cfg := megaCfg()
	cfg.CanaryURLs = []string{"https://localhost/cs"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	m := NewMega(cfg).(*mega)
	// Invalid handle -> the fake API returns -9; that means "the API is talking".
	res, err := m.Diagnose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(res) {
		t.Fatalf("FAIL on a healthy API: %+v", res)
	}
	got := map[Layer]bool{}
	for _, r := range res {
		got[r.Layer] = true
	}
	for _, l := range []Layer{LayerDNS, LayerTLS, LayerFetch, LayerParse} {
		if !got[l] {
			t.Errorf("the %s layer is missing from the report", l)
		}
	}
}

func TestMegaDiagnoseUnreachable(t *testing.T) {
	rt := &hostRouter{failures: map[string]error{"localhost": errors.New("connection refused")}}
	cfg := megaCfg()
	cfg.CanaryURLs = []string{"https://localhost/cs"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	m := NewMega(cfg).(*mega)
	res, _ := m.Diagnose(context.Background())
	if !hasFail(res) {
		t.Fatalf("expected FAIL on an unreachable API: %+v", res)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// mega's storage servers take the range in the path, end inclusive, the way
// MegaBasterd and go-mega ask for it.
func TestMegaRangeURL(t *testing.T) {
	m := NewMega(SiteConfig{Name: MegaName}.WithDefaults()).(RangeURLer)
	base := "http://gfs270n172.userstorage.mega.co.nz/dl/AbC-dEf_123"
	cases := []struct {
		start, end int64
		want       string
	}{
		{0, 1, base + "/0-0"},
		{1 << 20, 2 << 20, base + "/1048576-2097151"},
		{500, 0, base + "/500"}, // no end: to the end of the file
	}
	for _, c := range cases {
		if got := m.RangeURL(base, c.start, c.end); got != c.want {
			t.Errorf("RangeURL(%d, %d) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
	if got := m.RangeURL(base+"/", 0, 1); got != base+"/0-0" {
		t.Errorf("a trailing slash doubled: %q", got)
	}
}
