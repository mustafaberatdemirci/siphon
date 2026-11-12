package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/instance"
	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/server"
)

// runServe is "siphon serve": the window's download queue, driven from a
// browser (internal/server).
//
// It keeps its queue in its own data folder, never the window's: the two
// may run on one machine. One serve per data folder, decided by the same
// lock the window uses.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		addr      string
		outDir    string
		dataDir   string
		token     string
		cfgPath   string
		maxActive int
		unpack    bool
		unpackDel bool
		verbose   bool
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:8080", "address to listen on; :8080 for every network interface")
	fs.StringVar(&outDir, "out", ".", "download folder")
	fs.StringVar(&dataDir, "data", "", "folder for the queue and the token (default: Siphon's settings folder, \"server\")")
	fs.StringVar(&token, "token", "", "the token the page and the API need (default: $SIPHON_TOKEN, else one made and kept in the data folder)")
	fs.StringVar(&cfgPath, "c", "", "path to sites.toml")
	fs.IntVar(&maxActive, "max-active", queue.DefaultMaxActive, "downloads at once")
	fs.BoolVar(&unpack, "extract", false, "unpack finished archives")
	fs.BoolVar(&unpackDel, "extract-delete", false, "unpack finished archives and delete them after")
	fs.BoolVar(&verbose, "v", false, "also print detail")
	network := networkFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: siphon serve [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return run.ExitUsage
	}
	log := logger{verbose: verbose}

	if dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			log.errorf("no settings folder for the queue; give one with -data: %v", err)
			return run.ExitUsage
		}
		dataDir = filepath.Join(base, "Siphon", "server")
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.errorf("data folder: %v", err)
		return run.ExitUsage
	}
	out, err := filepath.Abs(outDir)
	if err == nil {
		err = os.MkdirAll(out, 0o755)
	}
	if err != nil {
		log.errorf("download folder: %v", err)
		return run.ExitUsage
	}
	if token == "" {
		token = os.Getenv("SIPHON_TOKEN")
	}
	if token == "" {
		if token, err = savedToken(dataDir); err != nil {
			log.errorf("token: %v", err)
			return run.ExitUsage
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var srv *server.Server
	notice := func(s string) {
		log.infof("%s", s)
		if srv != nil {
			srv.Notify(s)
		}
	}
	eng, err := queue.New(queue.Options{
		StatePath:    filepath.Join(dataDir, "queue.json"),
		ConfigPath:   cfgPath,
		MaxActive:    maxActive,
		Events:       log.events(),
		OnNotice:     notice,
		CheckNetwork: netcheck.Check,
		Network:      network(),
		OnQuotaHold: func(siteName string, retryAt time.Time) {
			notice(fmt.Sprintf("%s quota exceeded: downloads wait until %s, or go on by themselves once the server's IP changes.",
				siteName, retryAt.Format("15:04")))
		},
	})
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	if f := eng.UnreadableStateFile(); f != "" {
		log.errorf("the saved queue could not be read, so it starts empty; the file was kept as %s", f)
	}
	eng.SetUnpack(unpack || unpackDel, unpackDel)

	srv, err = server.New(ctx, eng, token, out, version)
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}

	// One serve per data folder. Links the browser integration hands over
	// would go to the window, which has its own folder; Add here takes them
	// should anything reach this one.
	release, running := instance.Claim(dataDir, instance.Handlers{Add: func(links []string) {
		for _, l := range links {
			if _, err := eng.Add(ctx, l, out); err != nil {
				notice("Could not add " + l + ": " + err.Error())
			}
		}
	}})
	if running {
		log.errorf("another siphon serve is using %s; give this one its own -data folder", dataDir)
		return run.ExitUsage
	}
	defer release()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	engineDone := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(engineDone)
	}()
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shut)
	}()

	log.infof("Siphon %s serving %s", version, out)
	log.infof("open %s", pageURL(ln.Addr(), token))
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.errorf("%v", err)
		stop()
		<-engineDone
		return run.ExitUsage
	}
	// The engine saves the queue on the way out; unfinished downloads go on
	// at the next start.
	<-engineDone
	return run.ExitOK
}

// pageURL is the address to open, with the token after # so the page signs
// in once (the part after # never reaches a server or a log).
func pageURL(a net.Addr, token string) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return "http://" + a.String() + "/"
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/#token=" + token
}

// savedToken is the token kept in the data folder, made the first time:
// 32 random bytes, readable by the user only.
func savedToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}
