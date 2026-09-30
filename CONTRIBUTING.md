# Contributing to Siphon

Thanks for helping. A few things make a change easy to take in:

- **Run the checks** before sending it: `go vet ./...` and
  `go test -race ./...` (the race detector needs a C compiler). CI runs them
  on Windows, Linux and macOS.
- **Tests run offline.** Resolvers are tested against local `httptest`
  servers that answer like the real site did when it was measured. Checks
  against the real sites are opt-in (`SIPHON_LIVE=1`) and use public,
  neutral links only; please keep it that way.
- **English** in code, comments, commit messages and the window's source
  texts. Texts shown in the window go through `T(...)` / `Tf(...)` and need a
  Turkish and a German entry; a test lists what is missing. New languages
  are welcome: see *Translations* in the README.
- **A new site** is a resolver in `internal/site`, an entry in
  `internal/config/sites.toml` (with what was measured, and when), offline
  tests, a live test with a neutral public link, and a `Diagnose` that
  checks the whole chain. The mediafire, gofile and cyberdrop resolvers are
  small examples to start from.
- **When a site breaks,** `siphon doctor <site> -v -record` saves the raw
  answers; comparing them with a test fixture usually shows what changed.

Siphon downloads what sites serve to anyone with the link. Changes that get
around logins, paywalls, DRM or captchas are out of scope.
