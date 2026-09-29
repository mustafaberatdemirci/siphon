package config

import _ "embed"

// Embedded holds the embedded default site definitions.
//
// The file lives under internal/config, not at the repo root: go:embed can
// only embed files from its own directory and below, and there are now two
// binaries (siphon.exe and siphon-gui.exe). Kept at the root, each binary
// would need its own copy and the two copies would drift apart over time.
//
// The external override mechanism is unchanged: the -c flag, next to the
// exe, the cwd.
//
//go:embed sites.toml
var Embedded []byte
