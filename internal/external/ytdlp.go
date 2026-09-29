package external

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Entry is one video yt-dlp found behind a link.
type Entry struct {
	URL   string // the video's own page: downloaded on its own, one queue job each
	Title string
	ID    string
	Size  int64 // -1 when unknown (yt-dlp often only estimates)
}

// ytInfo is the part of yt-dlp's -J output Siphon reads.
type ytInfo struct {
	Type           string    `json:"_type"`
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	WebpageURL     string    `json:"webpage_url"`
	Filesize       *float64  `json:"filesize"`
	FilesizeApprox *float64  `json:"filesize_approx"`
	Entries        []*ytInfo `json:"entries"`
}

func (i *ytInfo) size() int64 {
	switch {
	case i.Filesize != nil && *i.Filesize > 0:
		return int64(*i.Filesize)
	case i.FilesizeApprox != nil && *i.FilesizeApprox > 0:
		return int64(*i.FilesizeApprox)
	}
	return -1
}

// YtDlpProbe lists what is behind a link without downloading anything: one
// entry for a video, one per video for a playlist or channel (listed flat,
// so a 500-video playlist is one quick request, not 500). playlist is the
// playlist's title, "" for a single video. ErrUnsupported when yt-dlp
// doesn't recognize the link.
//
// MEASURED (yt-dlp 2026.08.19): an unknown link exits 1 with
// "ERROR: Unsupported URL: …" on stderr; a video gives _type "video" with
// webpage_url, title, id and filesize_approx.
func (t Tools) YtDlpProbe(ctx context.Context, link string) (playlist string, entries []Entry, err error) {
	if t.YtDlp == "" {
		return "", nil, ErrUnsupported
	}
	args := append(t.jsRuntime(), "--flat-playlist", "-J", "--no-warnings", "--encoding", "utf-8", "--", link)
	out, stderr, err := output(command(ctx, t.YtDlp, args...))
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if strings.Contains(stderr, "Unsupported URL") {
			return "", nil, ErrUnsupported
		}
		return "", nil, fmt.Errorf("yt-dlp: %s", lastLineWith(stderr, "ERROR"))
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", nil, fmt.Errorf("yt-dlp: unreadable answer: %w", err)
	}
	if info.Type != "playlist" && info.Type != "multi_video" {
		u := info.WebpageURL
		if u == "" {
			u = link
		}
		return "", []Entry{{URL: u, Title: info.Title, ID: info.ID, Size: info.size()}}, nil
	}
	for _, e := range info.Entries {
		if e == nil {
			continue
		}
		u := e.WebpageURL
		if u == "" {
			u = e.URL
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue // an id without a page; nothing to download on its own
		}
		entries = append(entries, Entry{URL: u, Title: e.Title, ID: e.ID, Size: e.size()})
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("yt-dlp: %q lists no videos", info.Title)
	}
	return info.Title, entries, nil
}

const (
	progressMark = "SIPHON-PROGRESS "
	fileMark     = "SIPHON-FILE "
)

// YtDlpDownload downloads one link into dir and returns the file yt-dlp
// produced (the last one, if the link held several) and the total size.
// progress may be nil; total is -1 when yt-dlp doesn't know it.
//
// Pausing is canceling ctx: the process is killed and its .part file stays;
// the next run continues it (yt-dlp's default).
//
// MEASURED: without ffmpeg, yt-dlp's default format downloads picture and
// sound as two separate files, doesn't merge them, and still reports the
// merged name, which never exists on disk. So without ffmpeg Siphon asks for
// the best single file ("-f b"), and checks that every file reported really
// is on disk. MEASURED: "b" takes the audio on an audio-only site
// (SoundCloud) and fails on a video that has no single file with both, while
// "b/ba" handed YouTube's audio alone to someone who asked for a video.
func (t Tools) YtDlpDownload(ctx context.Context, link, dir string, progress func(done, total int64)) (path string, size int64, err error) {
	if t.YtDlp == "" {
		return "", 0, ErrUnsupported
	}
	args := []string{
		"--newline", "--no-colors", "--no-playlist", "--progress", "--encoding", "utf-8",
		"--progress-template", "download:" + progressMark +
			"%(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
		"--print", "after_move:" + fileMark + "%(filepath)s",
		"-P", dir,
		"-o", "%(title).150B [%(id)s].%(ext)s",
	}
	if t.FFmpeg != "" {
		args = append(args, "--ffmpeg-location", t.FFmpeg)
	} else {
		args = append(args, "-f", "b")
	}
	args = append(args, t.jsRuntime()...)
	args = append(args, "--", link)

	var files []string
	stderr, err := run(command(ctx, t.YtDlp, args...), func(line string) {
		switch {
		case strings.HasPrefix(line, progressMark):
			if progress != nil {
				done, total := parseProgress(strings.TrimPrefix(line, progressMark))
				if done >= 0 {
					progress(done, total)
				}
			}
		case strings.HasPrefix(line, fileMark):
			files = append(files, strings.TrimSpace(strings.TrimPrefix(line, fileMark)))
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		if t.FFmpeg == "" && strings.Contains(stderr, "Requested format is not available") {
			return "", 0, fmt.Errorf("the site sends picture and sound separately, and merging them needs ffmpeg: install it with Diagnose > Install tools, or 'siphon tools install ffmpeg'")
		}
		return "", 0, fmt.Errorf("yt-dlp: %s", lastLineWith(stderr, "ERROR"))
	}
	if len(files) == 0 {
		return "", 0, fmt.Errorf("yt-dlp finished without reporting a file: %s", lastLineWith(stderr, "ERROR"))
	}
	for _, f := range files {
		fi, serr := os.Stat(f)
		if serr != nil || fi.IsDir() {
			return "", 0, fmt.Errorf("yt-dlp reported %s, but it isn't on disk (were picture and sound left unmerged? installing ffmpeg fixes that: Diagnose > Install tools)", f)
		}
		size += fi.Size()
	}
	return files[len(files)-1], size, nil
}

// parseProgress reads "<downloaded> <total> <estimate>"; yt-dlp writes "NA"
// for what it doesn't know. done is -1 if unreadable.
func parseProgress(s string) (done, total int64) {
	f := strings.Fields(s)
	num := func(i int) int64 {
		if i >= len(f) {
			return -1
		}
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil || v < 0 {
			return -1
		}
		return int64(v)
	}
	done, total = num(0), num(1)
	if total <= 0 {
		total = num(2)
	}
	return done, total
}

// jsRuntime tells yt-dlp where deno is when Siphon found it: yt-dlp only
// looks on PATH by itself, and "Install tools" puts deno in Siphon's folder.
func (t Tools) jsRuntime() []string {
	if t.Deno == "" {
		return nil
	}
	return []string{"--js-runtimes", "deno:" + t.Deno}
}
