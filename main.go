// Siphon downloads file-host links (mega, gofile, mediafire, pixeldrain,
// bunkr, cyberdrop) and plain file links in bulk.
//
// This file is only the command-line interface: flags, reading input, output
// format. The whole download pipeline lives in internal/run, and the window
// version (cmd/siphon-gui) calls the same pipeline; keeping the two
// interfaces from drifting apart depends on this separation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/external"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

type logger struct {
	verbose bool
	quiet   bool
}

func (l logger) infof(format string, a ...any) {
	if l.quiet {
		return
	}
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func (l logger) debugf(format string, a ...any) {
	if !l.verbose || l.quiet {
		return
	}
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func (l logger) errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// events wires the logger into run.Events.
func (l logger) events() run.Events {
	return run.Events{
		Debugf: l.debugf,
		Infof:  l.infof,
		Errorf: l.errorf,
		ConfigLoaded: func(src string, n int) {
			l.debugf("config: %s (%d sites)", src, n)
		},
		LedgerOpened: func(path string, items, _ int) {
			l.debugf("ledger: %s (%d items)", path, items)
		},
		ItemResolved: func(it site.Item) {
			// --resolve-only output: just the URL, one per line. Being
			// pipeable matters.
			fmt.Fprintln(os.Stdout, it.URL)
		},
	}
}

func main() {
	// The subcommand is split off BEFORE flag.Parse. Go's flag package stops
	// at the first non-flag argument, so the "siphon doctor -v" form can't be
	// parsed with a single FlagSet.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "tools" {
		os.Exit(runTools(os.Args[2:]))
	}
	os.Exit(runCLI())
}

func runCLI() int {
	var (
		inputPath   string
		outDir      string
		cfgPath     string
		verbose     bool
		quiet       bool
		resolveOnly bool
		onQuota     string
		showVersion bool
	)
	flag.StringVar(&inputPath, "i", "", "URL list file (one URL per line, # for comments)")
	flag.StringVar(&outDir, "out", ".", "output root")
	flag.StringVar(&cfgPath, "c", "", "path to sites.toml (if not given: next to the exe, then the cwd, then embedded)")
	flag.BoolVar(&verbose, "v", false, "also print layer details")
	flag.BoolVar(&quiet, "q", false, "only print errors")
	flag.BoolVar(&resolveOnly, "resolve-only", false, "print the resolved URLs, don't download")
	flag.StringVar(&onQuota, "on-quota", "", "command to run when a site's quota runs out (e.g. a script that switches VPN); continues when the allowance opens up")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: siphon [flags] [url ...]\n")
		fmt.Fprintf(os.Stderr, "       siphon doctor [flags] [site ...]\n")
		fmt.Fprintf(os.Stderr, "       siphon tools [install [tool ...]]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if showVersion {
		fmt.Println("siphon", version)
		return run.ExitOK
	}

	log := logger{verbose: verbose, quiet: quiet}

	// Ctrl+C: the context is canceled, the downloader syncs the .part and writes the state.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	urls, err := readURLs(inputPath, flag.Args())
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	if len(urls) == 0 {
		log.errorf("no input: give a file with -i or pass URLs as arguments")
		flag.Usage()
		return run.ExitUsage
	}

	sum, err := run.Run(ctx, run.Options{
		URLs:        urls,
		OutDir:      outDir,
		ConfigPath:  cfgPath,
		ResolveOnly: resolveOnly,
		OnQuota:     onQuota,
	}, log.events())
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	return sum.ExitCode()
}

// runDoctor runs the layer diagnosis.
//
// Exit code: 0 no FAIL, 1 at least one FAIL, 3 configuration error.
// WARN does NOT AFFECT the exit code, and that is deliberate: the "CDN is a
// signal, not a gate" rule only means something if WARN doesn't count as a
// failure. Seeing a new CDN host must not break a script; it should tell the
// user.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var (
		cfgPath   string
		verbose   bool
		quiet     bool
		record    bool
		recordDir string
	)
	fs.StringVar(&cfgPath, "c", "", "path to sites.toml")
	fs.BoolVar(&verbose, "v", false, "also print diagnostic detail")
	fs.BoolVar(&quiet, "q", false, "only print errors")
	fs.BoolVar(&record, "record", false, "save the responses to disk (for diffing)")
	fs.StringVar(&recordDir, "record-dir", doctor.DefaultDir, "recording folder")
	// Why a flag is needed: canary_urls is an array field and array fields
	// are MERGED when configs are combined (union, order preserved). Writing
	// a canary into an external file appends it to the END of the list, and
	// since Diagnose stops at the first working canary it is never reached.
	// The merge semantics are right for domains (you want to add, not
	// replace), but there has to be a way to ask "is this album broken".
	var canaries stringList
	fs.Var(&canaries, "canary", "replace the canary URL (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: siphon doctor [flags] [site ...]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return run.ExitUsage
	}

	log := logger{verbose: verbose, quiet: quiet}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var rec *doctor.Recorder
	var recFn func(string) func(string, []byte)
	if record {
		rec = &doctor.Recorder{Dir: recordDir}
		recFn = rec.For
	}

	cfgs, resolvers, err := run.Setup(log.events(), cfgPath, recFn, canaries, nil)
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}

	// If arguments are given only those sites are diagnosed.
	want := map[string]bool{}
	for _, a := range fs.Args() {
		want[strings.ToLower(a)] = true
	}
	var sites []doctor.Named
	for i, r := range resolvers {
		if _, fallback := r.(site.Fallback); fallback {
			continue // plain file links: no site to diagnose
		}
		name := cfgs[i].Name
		if len(want) > 0 && !want[strings.ToLower(name)] {
			continue
		}
		sites = append(sites, doctor.Named{Name: name, Resolver: r})
	}
	if len(sites) == 0 {
		log.errorf("no site to diagnose (known: %s)", strings.Join(siteNames(cfgs), ", "))
		return run.ExitUsage
	}

	reports := doctor.Run(ctx, sites)
	worst := doctor.Format(os.Stdout, reports)
	if len(want) == 0 {
		printTools(os.Stdout, cfgs)
	}

	if rec != nil {
		for _, f := range rec.Saved() {
			log.infof("saved: %s", f)
		}
		// A recording error does NOT fail the diagnosis: the real job is the layer report.
		for _, e := range rec.Errs() {
			log.errorf("recording error: %v", e)
		}
		if len(rec.Saved()) == 0 && len(rec.Errs()) == 0 {
			log.errorf("recording was requested but no response was saved")
		}
	}

	if worst == site.StatusFail {
		return run.ExitPartial
	}
	return run.ExitOK
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty canary")
	}
	*l = append(*l, v)
	return nil
}

func siteNames(cfgs []site.SiteConfig) []string {
	out := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		if c.Name == site.DirectName {
			continue // plain file links, not a site to diagnose
		}
		out = append(out, c.Name)
	}
	return out
}

// readURLs combines the -i file and the positional arguments.
// Blank lines and lines starting with # are skipped.
func readURLs(path string, args []string) ([]string, error) {
	out := append([]string{}, args...)
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: could not read the -i file: %v", config.ErrUsage, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// printTools lists the external tools Siphon hands other sites' pages to
// (sites.toml's "direct" entry may say where they are).
func printTools(w io.Writer, cfgs []site.SiteConfig) {
	var extra map[string]string
	for _, c := range cfgs {
		if c.Name == site.DirectName {
			extra = c.Extra
		}
	}
	fmt.Fprintln(w, "\ntools (for pages of sites Siphon doesn't know)")
	for _, l := range external.Find(extra).Lines() {
		fmt.Fprintln(w, l)
	}
}
