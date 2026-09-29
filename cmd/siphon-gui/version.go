package main

// version is set at build time by the release workflow
// (-ldflags "-X main.version=v1.2.3"); a local build says "dev".
var version = "dev"
