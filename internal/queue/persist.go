package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateVersion, kuyruk dosyasının biçim sürümü. Bilinmeyen sürüm okunmaz:
// sessizce yanlış yorumlamaktansa boş kuyrukla başlamak ve söylemek daha iyi.
const stateVersion = 1

type stateFile struct {
	Version int    `json:"version"`
	Jobs    []*Job `json:"jobs"`
}

// load, kuyruk dosyasını okur. Dosya yoksa boş liste ve nil hata.
func load(path string) ([]*Job, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kuyruk okunamadı: %w", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("kuyruk dosyası bozuk: %w", err)
	}
	if sf.Version != stateVersion {
		return nil, fmt.Errorf("kuyruk dosyası sürüm %d, bu sürüm %d bekliyor", sf.Version, stateVersion)
	}
	// Kapanış anında "running" olan işler kaldığı yerden DEVAM ETMELİ:
	// kullanıcı durdurmadı, uygulama kapandı. Kuyruğa geri konuyor.
	for _, j := range sf.Jobs {
		if j.State == StateRunning {
			j.State = StateQueued
		}
	}
	return sf.Jobs, nil
}

// save, kuyruğu ATOMİK yazar: geçici dosya + rename. Yarım yazılmış bir
// kuyruk dosyası bir sonraki açılışta tüm listeyi kaybettirirdi.
func save(path string, jobs []*Job) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultStatePath, kuyruk dosyasının varsayılan yeri: kullanıcının config
// klasörü (Windows'ta %AppData%\Siphon). Çıktı klasörüne yazılmıyor: kuyruk
// uygulama seviyesinde, işler farklı çıktı klasörlerine gidebilir.
func DefaultStatePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Siphon", "queue.json"), nil
}
