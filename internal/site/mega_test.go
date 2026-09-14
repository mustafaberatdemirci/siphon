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
	"strings"
	"sync"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
)

// --- Sahte mega API'si ---
//
// Gercek kriptoyu kullaniyor: oznitelikler gercekten sifreleniyor, dugum
// anahtarlari gercekten klasor anahtariyla sariliyor. Boylece resolver'in
// cozdugu sey, sahte sunucunun "dogru" dedigi sey degil, kriptonun sonucu.

type megaFakeFile struct {
	packed []byte // 32 bayt paketli anahtar
	name   string
	size   int64
}

type megaFakeNode struct {
	handle, parent, name string
	isFolder             bool
	packed               []byte // dosya: 32, klasor: 16
	size                 int64
}

type megaFakeFolder struct {
	key   []byte // 16
	nodes []megaFakeNode
	// root, kok klasorun DUGUM handle'i. Gercek API'de bu, linkteki
	// (paylasim) handle'indan HER ZAMAN farkli; "k" etiketleri ve "p"
	// zinciri bununla calisir. Bos birakilirsa ilk dugum kok sayilir.
	root string
	// foreign, sahibinin ayni agaci daha ustten de paylastigi durumu
	// canlandirir: her dugumun "k" alaninda ONCE bu paylasimin (bizde
	// anahtari olmayan) girdisi, sonra bizimki gelir. Canli gozlem.
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
	calls    []string // "g:p=<h>", "g:n=<h>", "f"
	bareErrs []int    // siradaki cagrilarda tum govde olarak donecek hata kodlari
	nodeErr  map[string]int
}

func newMegaFakeAPI(t *testing.T) *megaFakeAPI {
	return &megaFakeAPI{
		t: t, files: map[string]megaFakeFile{}, folders: map[string]megaFakeFolder{},
		cdn: "https://cdn.test", nodeErr: map[string]int{},
	}
}

func (f *megaFakeAPI) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("mega API metodu = %s", r.Method)
	}
	body, _ := io.ReadAll(r.Body)
	var cmds []map[string]any
	if err := json.Unmarshal(body, &cmds); err != nil {
		f.t.Errorf("API govdesi JSON dizi degil: %v", err)
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
				// Gercek API: etiket = kok DUGUM handle'i (link handle'i degil);
				// kok dugum de kendi "k"sini tasir.
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

// packedKeyFor, duz metne uygun (dogru meta-MAC'li) paketli anahtar uretir:
// linkte gelen 32 baytin birebir karsiligi.
func packedKeyFor(t *testing.T, plain []byte) []byte {
	t.Helper()
	aesKey, nonce := randBytes(t, 16), randBytes(t, 8)
	mac, err := megacrypto.MetaMACOf(aesKey, nonce, plain)
	if err != nil {
		t.Fatal(err)
	}
	return megacrypto.PackFileKey(aesKey, nonce, mac)
}

// --- Link tanima ---

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
		"https://mega.nz/file/AbCdEfGh",                             // anahtar yok
		"https://mega.nz/file/AbCdEfGh#" + folderKey,                // dosya icin 16 bayt
		"https://mega.nz/folder/FoLdErHa#" + fileKey,                // klasor icin 32 bayt
		"https://mega.nz/file/AbCdEfGh#" + fileKey[:len(fileKey)-5], // kirpilmis
		"https://mega.attacker.com/file/AbCdEfGh#" + fileKey,
	}
	for _, in := range bad {
		if m.Match(in) {
			t.Errorf("Match(%q) = true, false bekleniyordu", in)
		}
	}
}

// --- Tek dosya ---

func TestMegaResolveFileAndDecode(t *testing.T) {
	plain := randBytes(t, 200*1024+9)
	packed := packedKeyFor(t, plain)
	api := newMegaFakeAPI(t)
	api.files["FiLeHaNd"] = megaFakeFile{packed: packed, name: "Tatil — Özgür.mp4", size: int64(len(plain))}
	m := newMegaWith(t, api)

	link := "https://mega.nz/file/FiLeHaNd#" + megacrypto.B64Encode(packed)
	var items []Item
	itemErrs, err := m.Resolve(context.Background(), link, func(it Item) error { items = append(items, it); return nil })
	if err != nil || len(itemErrs) != 0 {
		t.Fatalf("Resolve: %v / %v", err, itemErrs)
	}
	if len(items) != 1 {
		t.Fatalf("%d item, 1 bekleniyordu", len(items))
	}
	it := items[0]
	if it.Filename != "Tatil — Özgür.mp4" || it.Size != int64(len(plain)) {
		t.Errorf("item = %+v", it)
	}
	if it.URL != "https://cdn.test/dl/FiLeHaNd" {
		t.Errorf("URL = %q", it.URL)
	}
	if it.SourcePage != link {
		t.Errorf("SourcePage = %q, kanonik link bekleniyordu", it.SourcePage)
	}
	if !bytes.Equal(it.Secret, packed) {
		t.Error("Secret paketli anahtari tasimiyor")
	}

	// Cozucu: sifreli govdeyi verince duz metin cikmali ve Verify gecmeli.
	key, _ := megacrypto.UnpackFileKey(packed)
	enc, _ := megacrypto.EncryptCTR(key.AES, key.Nonce, plain)
	ds, err := m.DecodeStream(it, 0, nil, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	got, _ := io.ReadAll(ds)
	if !bytes.Equal(got, plain) {
		t.Fatal("cozulen icerik duz metin degil")
	}
	if err := ds.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestMegaDecodeStreamRejectsMissingSecret(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	if _, err := m.DecodeStream(Item{}, 0, nil, bytes.NewReader(nil)); err == nil {
		t.Fatal("Secret'siz item icin cozucu kuruldu")
	}
}

// --- Klasor ---

func setupFolder(t *testing.T, api *megaFakeAPI) (folderKey []byte, packedA, packedB, packedC []byte) {
	t.Helper()
	folderKey = randBytes(t, 16)
	rootKey := randBytes(t, 16) // paylasim anahtari != kokun kendi anahtari (canli gozlem)
	subKey := randBytes(t, 16)
	packedA = packedKeyFor(t, []byte("a"))
	packedB = packedKeyFor(t, []byte("b"))
	packedC = packedKeyFor(t, []byte("c"))
	// Link handle'i "FoLdErHa", kok DUGUM "RoOtNoDe", kokun ebeveyni
	// "OwNeRdIr" (sahibinin hesabinda, listede yok) — canli API'nin sekli.
	api.folders["FoLdErHa"] = megaFakeFolder{key: folderKey, root: "RoOtNoDe", nodes: []megaFakeNode{
		{handle: "RoOtNoDe", parent: "OwNeRdIr", name: "Kök Klasör", isFolder: true, packed: rootKey},
		{handle: "SuBfOlDr", parent: "RoOtNoDe", name: "Alt", isFolder: true, packed: subKey},
		{handle: "NoDeAAAA", parent: "RoOtNoDe", name: "a.mp4", packed: packedA, size: 100},
		{handle: "NoDeBBBB", parent: "RoOtNoDe", name: "b.mp4", packed: packedB, size: 200},
		{handle: "NoDeCCCC", parent: "SuBfOlDr", name: "c.mp4", packed: packedC, size: 300},
	}}
	return
}

// ÖLÇÜLDÜ (mega.nz/folder/VVplxTBY): sahibi agaci daha ustten de
// paylasmissa her dugumun "k" alaninda once o paylasimin girdisi gelir.
// Eski kod "link handle'iyla eslesen, yoksa ILK" diyordu; link handle'i
// hicbir zaman dugum handle'i olmadigi icin hep yabanci anahtari secti ve
// 373 dosyanin tamami "ad cozulemedi" diye atlandi. Kuyruk bos kaldi.
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
		t.Fatalf("item hatalari: %v", itemErrs)
	}
	if len(got) != 3 {
		t.Fatalf("%d item cozuldu, 3 bekleniyordu", len(got))
	}
	byName := map[string]Item{}
	for _, it := range got {
		byName[it.Filename] = it
	}
	// Klasor adlari da dogru anahtarla cozulmeli; kok, link anahtariyla.
	if d := byName["c.mp4"].Dir; d != "Kök Klasör/Alt" {
		t.Errorf("c.mp4 dizini = %q", d)
	}
	if d := byName["a.mp4"].Dir; d != "Kök Klasör" {
		t.Errorf("a.mp4 dizini = %q", d)
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
		t.Fatalf("%d item, 3 bekleniyordu", len(items))
	}
	byName := map[string]Item{}
	for _, it := range items {
		byName[it.Filename] = it
	}
	if a := byName["a.mp4"]; a.Dir != "Kök Klasör" || a.Size != 100 || !bytes.Equal(a.Secret, packedA) {
		t.Errorf("a = %+v", a)
	}
	// Alt klasordeki dosya: yol "Kok/Alt" (indirici "-" ile duzlestirecek).
	if c := byName["c.mp4"]; c.Dir != "Kök Klasör/Alt" || !bytes.Equal(c.Secret, packedC) {
		t.Errorf("c = %+v", c)
	}
	if byName["a.mp4"].SourcePage != link+"/file/NoDeAAAA" {
		t.Errorf("SourcePage = %q", byName["a.mp4"].SourcePage)
	}
	// Indeksler sirali olmali (ad cakismasi haritasi buna bagli).
	seen := map[int]bool{}
	for _, it := range items {
		seen[it.Index] = true
	}
	if !seen[0] || !seen[1] || !seen[2] {
		t.Errorf("indeksler 0,1,2 olmali: %v", seen)
	}
	// Klasor listesi 1, indirme adresleri TOPLU 1 cagri olmali.
	if api.count("f") != 1 || api.count("g:n=") != 3 {
		t.Errorf("cagrilar: %v", api.calls)
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
		t.Fatalf("alt klasor secimi yanlis: %+v", items)
	}
}

// ResolveOne, onbellegi bos bir resolver'da bile calismali (surec yeniden
// basladi): klasoru listeleyip dugumu bulur.
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
		t.Errorf("soguk onbellekte klasor 1 kez listelenmeliydi: %v", api.calls)
	}
	// Ikinci cagri onbellekten: yeniden listeleme YOK.
	if _, err := m.ResolveOne(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if api.count("f") != 1 {
		t.Errorf("sicak onbellekte klasor tekrar listelendi: %v", api.calls)
	}
}

func TestMegaResolveOneRejectsBareFolder(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	_, err := m.ResolveOne(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(make([]byte, 16)))
	if err == nil {
		t.Fatal("dugumsuz klasor linki tek item olarak cozuldu")
	}
}

// 120 dosya: indirme adresleri 50'lik paketlerle 3 cagrida alinmali.
func TestMegaFolderBatchesGetCalls(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey := randBytes(t, 16)
	nodes := []megaFakeNode{{handle: "RoOtNoDe", parent: "OwNeRdIr", name: "Kök", isFolder: true, packed: folderKey}}
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
		t.Fatalf("%d item, 120 bekleniyordu", n)
	}
	// hostRouter.seen uzerinden POST sayisi: 1 listeleme + 3 toplu g.
	posts := 0
	rt := m.cfg.HTTPClient.Transport.(*hostRouter)
	for _, s := range rt.seen {
		if strings.HasPrefix(s, "api.test") {
			posts++
		}
	}
	if posts != 4 {
		t.Errorf("%d API istegi, 4 bekleniyordu (1 liste + 3 toplu)", posts)
	}
}

// --- Hata haritasi ---

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
			t.Errorf("kod %d -> %v, %v bekleniyordu", c.code, l, c.layer)
		}
	}
}

func TestMegaFileNotFound(t *testing.T) {
	api := newMegaFakeAPI(t)
	m := newMegaWith(t, api)
	_, err := m.Resolve(context.Background(), "https://mega.nz/file/YoKdOsYa#"+megacrypto.B64Encode(make([]byte, 32)),
		func(Item) error { return nil })
	if l, ok := LayerOf(err); !ok || l != LayerItemPage {
		t.Fatalf("olmayan dosya icin %v, ItemPage bekleniyordu", err)
	}
}

// Gecici hata (-3) birkac kez denenmeli; ikinci denemede basari.
func TestMegaTransientErrorIsRetried(t *testing.T) {
	plain := []byte("veri")
	packed := packedKeyFor(t, plain)
	api := newMegaFakeAPI(t)
	api.files["FiLeHaNd"] = megaFakeFile{packed: packed, name: "x.bin", size: 4}
	api.bareErrs = []int{-3}
	m := newMegaWith(t, api)

	n := 0
	_, err := m.Resolve(context.Background(), "https://mega.nz/file/FiLeHaNd#"+megacrypto.B64Encode(packed),
		func(Item) error { n++; return nil })
	if err != nil || n != 1 {
		t.Fatalf("gecici hata sonrasi basari bekleniyordu: %v (n=%d)", err, n)
	}
}

// Kota (-17) GECICI DEGIL: tekrar denenmemeli, aninda ve net bildirilmeli.
func TestMegaQuotaIsNotRetried(t *testing.T) {
	api := newMegaFakeAPI(t)
	api.bareErrs = []int{-17, -17, -17, -17}
	m := newMegaWith(t, api)

	_, err := m.Resolve(context.Background(), "https://mega.nz/file/FiLeHaNd#"+megacrypto.B64Encode(make([]byte, 32)),
		func(Item) error { return nil })
	if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Fatalf("kota icin %v, CDN katmani bekleniyordu", err)
	}
	if !strings.Contains(err.Error(), "kota") {
		t.Errorf("hata mesaji kotayi soylemiyor: %v", err)
	}
	if len(api.bareErrs) != 3 {
		t.Errorf("kota hatasi tekrar denendi: kalan %d", len(api.bareErrs))
	}
}

// Klasorde tek bir dugum hata verirse album DUSMEMELI, ItemError toplanmali.
func TestMegaFolderCollectsPerItemErrors(t *testing.T) {
	api := newMegaFakeAPI(t)
	folderKey, _, _, _ := setupFolder(t, api)
	api.nodeErr["NoDeBBBB"] = -16
	m := newMegaWith(t, api)

	n := 0
	itemErrs, err := m.Resolve(context.Background(), "https://mega.nz/folder/FoLdErHa#"+megacrypto.B64Encode(folderKey),
		func(Item) error { n++; return nil })
	if err != nil {
		t.Fatalf("klasor dusmemeliydi: %v", err)
	}
	if n != 2 || len(itemErrs) != 1 {
		t.Fatalf("n=%d itemErrs=%d; 2 ve 1 bekleniyordu", n, len(itemErrs))
	}
	if l, _ := LayerOf(itemErrs[0].Err); l != LayerItemPage {
		t.Errorf("engelli dosya katmani = %v", l)
	}
}

// --- Kota (HTTP 509) ---

func TestMegaClassifyStatus509(t *testing.T) {
	m := NewMega(megaCfg()).(*mega)
	resp := &http.Response{StatusCode: 509}
	err := m.ClassifyStatus(resp, nil)
	if l, ok := LayerOf(err); !ok || l != LayerCDN {
		t.Fatalf("509 -> %v, CDN bekleniyordu", err)
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("kota hatasi yeniden denenebilir isaretlendi")
	}
	// 403 nil donmeli: indirici onu "adres suresi doldu" sayip yeniler.
	if err := m.ClassifyStatus(&http.Response{StatusCode: 403}, nil); err != nil {
		t.Errorf("403 icin nil bekleniyordu: %v", err)
	}
}

// --- Teshis ---

func TestMegaDiagnoseHealthy(t *testing.T) {
	api := newMegaFakeAPI(t)
	// Diagnose gercek DNS sorgusu yapiyor; cozulebilen tek sahte host localhost.
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{"localhost": api.handler}}
	cfg := megaCfg()
	cfg.CanaryURLs = []string{"https://localhost/cs"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	m := NewMega(cfg).(*mega)
	// Gecersiz tanitici -> sahte API -9 dondurur; bu "API konusuyor" demek.
	res, err := m.Diagnose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hasFail(res) {
		t.Fatalf("saglikli API'de FAIL: %+v", res)
	}
	got := map[Layer]bool{}
	for _, r := range res {
		got[r.Layer] = true
	}
	for _, l := range []Layer{LayerDNS, LayerTLS, LayerFetch, LayerParse} {
		if !got[l] {
			t.Errorf("%s katmani raporda yok", l)
		}
	}
}

func TestMegaDiagnoseUnreachable(t *testing.T) {
	rt := &hostRouter{failures: map[string]error{"localhost": errors.New("baglanti reddedildi")}}
	cfg := megaCfg()
	cfg.CanaryURLs = []string{"https://localhost/cs"}
	cfg.HTTPClient = &http.Client{Transport: rt}
	m := NewMega(cfg).(*mega)
	res, _ := m.Diagnose(context.Background())
	if !hasFail(res) {
		t.Fatalf("ulasilamayan API'de FAIL bekleniyordu: %+v", res)
	}
}
