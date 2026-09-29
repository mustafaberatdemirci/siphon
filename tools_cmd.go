package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/external"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
)

// runTools is "siphon tools": which external tools were found, and
// "siphon tools install [tool ...]" to install or update them from their
// official releases (checked against the published SHA-256). Without names
// it installs the ones that are missing.
func runTools(args []string) int {
	found := external.Find(run.ToolPaths(""))
	if len(args) == 0 {
		fmt.Println("tools (for pages of sites Siphon doesn't know)")
		for _, l := range found.Lines() {
			fmt.Println(l)
		}
		if dir, err := external.ToolsDir(); err == nil {
			fmt.Printf("\n\"siphon tools install\" puts missing ones in %s\n", dir)
		}
		return run.ExitOK
	}
	if args[0] != "install" {
		fmt.Fprintf(os.Stderr, "usage: siphon tools [install [yt-dlp|ffmpeg|gallery-dl|deno ...]]\n")
		return run.ExitUsage
	}

	chosen, err := chooseTools(external.Packages(runtime.GOOS, runtime.GOARCH), found, args[1:], runtime.GOOS+"/"+runtime.GOARCH)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return run.ExitUsage
	}
	if len(chosen) == 0 {
		fmt.Println("every tool Siphon can install is already there; name one to update it, e.g. \"siphon tools install yt-dlp\"")
		return run.ExitOK
	}

	dir, err := external.ToolsDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no folder for the tools: %v\n", err)
		return run.ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := snet.NewClient()
	failed := 0
	for _, p := range chosen {
		if err := p.Resolve(ctx, client); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			failed++
			continue
		}
		fmt.Printf("%s: %s (%.1f MB)\n", p.Tool, p.URL, float64(p.Size)/(1<<20))
		last := -1
		err := external.Install(ctx, client, dir, p, func(done, total int64) {
			if total > 0 {
				if pct := int(done * 100 / total); pct/10 != last/10 {
					last = pct
					fmt.Printf("  %d%%\n", pct)
				}
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			failed++
			continue
		}
		fmt.Printf("  installed, SHA-256 checked (%s)\n", p.SHA256)
	}
	if failed > 0 {
		return run.ExitPartial
	}
	return run.ExitOK
}

// chooseTools picks what "siphon tools install" installs: the named tools
// (install or update), or with no names every tool that is missing.
func chooseTools(pkgs []external.Package, found external.Tools, names []string, system string) ([]external.Package, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}
	named := len(want) > 0 // decided once: emptying want below must not mean "no names"
	var chosen []external.Package
	for _, p := range pkgs {
		if named && want[p.Tool] || !named && found.Path(p.Tool) == "" {
			chosen = append(chosen, p)
		}
		delete(want, p.Tool)
	}
	for name := range want {
		switch name {
		case "yt-dlp", "ffmpeg", "gallery-dl", "deno":
			return nil, fmt.Errorf("%s can't be installed by Siphon on %s; use your package manager", name, system)
		default:
			return nil, fmt.Errorf("unknown tool %q (yt-dlp, ffmpeg, gallery-dl, deno)", name)
		}
	}
	return chosen, nil
}
