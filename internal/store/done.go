// Package store, tamamlanan indirmelerin append-only kaydını tutar.
//
// Kayıt dosyası ÇIKTI KÖKÜNÜN ALTINDA durur, exe'nin yanında veya cwd'de değil.
// Gerekçe: aksi halde farklı bir -out ile yapılan ikinci koşu, dosyalar orada
// olmadığı halde "hepsi zaten indi" deyip hiçbir şey indirmeden 0 döner. Bu,
// aracın "sessiz başarısızlık yok" ilkesinin doğrudan ihlali olurdu. Kayıt
// çıktının yanında durduğunda, çıktıyı taşıdığında kayıt da taşınıyor.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName, kayıt dosyasının adı.
const FileName = "done.jsonl"

// Entry, done.jsonl'daki bir satır.
type Entry struct {
	// Anahtar parçaları. Dir ve Filename HAM değerlerdir (resolver'ın verdiği),
	// diskteki temizlenmiş hali değil: anahtar indirme başlamadan önce
	// hesaplanabilmek zorunda.
	SourcePage string `json:"source_page"`
	Dir        string `json:"dir"`
	Filename   string `json:"filename"`

	// Path, çıktı köküne GÖRELİ gerçek yol. Şemada doküman bunu saymıyordu ama
	// onsuz "kayıt indi diyor ama dosya nerede" sorusu cevaplanamaz: kullanıcı
	// dosyayı silerse kayda güvenip atlamak sessiz başarısızlık olur.
	// Göreli tutuluyor ki çıktı klasörü taşındığında kayıt geçerli kalsın.
	Path string `json:"path"`

	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	TS     string `json:"ts"`
}

// Key, bir item'ın kayıt anahtarını üretir.
//
// Anahtar Item.URL'i İÇERMEZ. bunkr'ın imzalı CDN adresi her koşuda değişiyor
// (XOR anahtarı saatlik pencereye bağlı); URL ile anahtarlamak idempotent
// yeniden başlatmayı her seferinde bozardı.
//
// Alanlar uzunluk önekiyle karıştırılıyor: düz birleştirme ("a"+"bc" ile
// "ab"+"c") iki farklı item'ı aynı anahtara düşürebilirdi ve dosya adları her
// karakteri içerebiliyor, yani güvenli bir ayırıcı yok.
func Key(dir, sourcePage, filename string) string {
	h := sha256.New()
	for _, s := range []string{dir, sourcePage, filename} {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Ledger, append-only kayıt dosyası.
type Ledger struct {
	path string

	mu      sync.Mutex
	done    map[string]Entry
	f       *os.File
	skipped int
}

// Open, çıktı kökündeki kaydı okur ve ekleme için açar.
// Dosya yoksa boş bir kayıtla başlar.
func Open(outRoot string) (*Ledger, error) {
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return nil, fmt.Errorf("çıktı kökü açılamadı: %w", err)
	}
	path := filepath.Join(outRoot, FileName)

	l := &Ledger{path: path, done: map[string]Entry{}}
	if err := l.load(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("%s açılamadı: %w", path, err)
	}
	l.f = f
	return l, nil
}

func (l *Ledger) load() error {
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s okunamadı: %w", l.path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Uzun dosya adları ve yollar varsayılan 64 KB sınırını aşabilir.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.Filename == "" {
			// Çökme anında yarım yazılmış son satır beklenen durum. Kayıt
			// dosyasını bozuk sayıp koşuyu düşürmek, kurtarılabilir bir durumu
			// ölümcül yapmak olurdu. Ama sessiz de geçilmiyor: sayılıyor.
			l.skipped++
			continue
		}
		l.done[Key(e.Dir, e.SourcePage, e.Filename)] = e
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s taranamadı: %w", l.path, err)
	}
	return nil
}

// Lookup, item daha önce indirildiyse kaydını döndürür.
func (l *Ledger) Lookup(dir, sourcePage, filename string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.done[Key(dir, sourcePage, filename)]
	return e, ok
}

// Add, yeni bir satır ekler ve diske yazar.
//
// Her satırda fsync yapılıyor: indirilen dosyalar gigabayt ölçeğinde, dosya
// başına bir fsync ölçülemeyecek kadar küçük bir maliyet. Karşılığında
// elektrik kesintisinde kayıt, indirilen dosyayla tutarlı kalıyor.
func (l *Ledger) Add(e Entry) error {
	if e.Filename == "" {
		return errors.New("kayıt için dosya adı zorunlu")
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("kayıt kapalı")
	}
	if _, err := l.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("%s yazılamadı: %w", l.path, err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("%s sync edilemedi: %w", l.path, err)
	}
	l.done[Key(e.Dir, e.SourcePage, e.Filename)] = e
	return nil
}

// Len, kayıttaki benzersiz item sayısı.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.done)
}

// Skipped, okunamayan satır sayısı. Sıfır değilse kullanıcıya söylenmeli.
func (l *Ledger) Skipped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.skipped
}

// Path, kayıt dosyasının yolu.
func (l *Ledger) Path() string { return l.path }

func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
