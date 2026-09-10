package config

import _ "embed"

// Embedded, gömülü varsayılan site tanımları.
//
// Dosya internal/config altında duruyor, depo kökünde değil: go:embed yalnızca
// kendi klasöründen ve altından dosya gömebiliyor, ve artık iki binary var
// (siphon.exe ve siphon-gui.exe). Kökte tutulsaydı her binary için ayrı bir
// kopya gerekirdi ve iki kopya zamanla ayrışırdı.
//
// Dış override mekanizması değişmedi: -c bayrağı, exe'nin yanı, cwd.
//
//go:embed sites.toml
var Embedded []byte
