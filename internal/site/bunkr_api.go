package site

// bunkr: the file API, signing and decrypting the download URL.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------- API and decryption ----------

// bunkrAPIResponse is the download API's response. BOTH shapes are handled.
//
// The current shape (dl.bunkr.cr) gives the URL in pieces: mediafiles + path.
// The old shape (apidl.bunkr.ru) gives a full URL encrypted with XOR and in
// the 2026-09-10 measurement returned a STALE path: for the same file it said
// ".../file.m4v" where the current API says ".../storage/media/...". The old
// branch stays only for backward compatibility.
type bunkrAPIResponse struct {
	MediaFiles string `json:"mediafiles"`
	Path       string `json:"path"`
	Original   string `json:"original"`

	URL       string `json:"url"`
	Encrypted bool   `json:"encrypted"`
	Timestamp int64  `json:"timestamp"`
}

// bunkrSignResponse is the signing service's response.
type bunkrSignResponse struct {
	Token string `json:"token"`
	Ex    int64  `json:"ex"`
}

// redactToken deletes the token from a signing response before it is written
// to disk with --record.
//
// The token is a time-limited credential granting access to the file, and
// recordings are written with 0644. Diagnosis needs the SHAPE of the
// response, not the value: keeping the length is enough to answer "did a
// token arrive, is it plausible".
func redactToken(body []byte) []byte {
	var sig bunkrSignResponse
	if err := json.Unmarshal(body, &sig); err != nil {
		// If it can't be parsed it isn't in the expected shape; rather than
		// assuming it contains a token and hiding it all, we pass the raw body
		// through: error bodies (HTML, plain text) are valuable for diagnosis.
		return body
	}
	out, err := json.Marshal(struct {
		TokenLen int   `json:"token_len"`
		Ex       int64 `json:"ex"`
	}{len(sig.Token), sig.Ex})
	if err != nil {
		return nil
	}
	return out
}

// resolveFileURL resolves the real download URL for a data id.
//
// Referer and Origin are MANDATORY: the endpoint checks them and rejects the
// request if they are missing. gallery-dl's 2025-02-27 commit added both
// together. The SIGNATURE IS NOT TAKEN HERE. The URL is returned raw; the
// signature is fetched with PrepareURL when the download starts.
//
// Why: the token lives 2 hours (measured) but every item of an album would be
// signed at resolution time, while downloading only runs a few files at a
// time. On a slow connection the token of the file at the end of the queue
// dies before its turn and recovery costs three requests. Signing files that
// are skipped (already downloaded) or only listed was pure waste, too.
func (b *bunkr) resolveFileURL(ctx context.Context, dataID string) (string, string, error) {
	referer := b.dlOrigin + "/file/" + url.PathEscape(dataID)

	// If the primary endpoint is marked dead it is NOT TRIED until
	// primaryRetryAfter passes. Trying meant waiting a separate timeout for
	// every file of an album: on a 40-file album the UI froze for minutes.
	primaryErr := error(nil)
	if !b.primaryIsDead() {
		data, err := b.callAPI(ctx, b.apiEndpoint, dataID, referer)
		if err == nil {
			rawURL, rerr := b.rawFileURL(data, dataID)
			if rerr != nil {
				return "", "", rerr
			}
			return rawURL, referer, nil
		}
		// Fall back ONLY when the primary endpoint is UNREACHABLE. Real
		// answers such as "file deleted" (400) aren't fixed by the fallback.
		if !b.markPrimaryDead(err) {
			return "", "", err
		}
		primaryErr = err
	}

	fb, ferr := b.callAPI(ctx, b.fallbackAPI, dataID, referer)
	if ferr != nil {
		// The primary error is more informative: the real problem is that it
		// can't be reached.
		if primaryErr != nil {
			return "", "", primaryErr
		}
		return "", "", ferr
	}
	raw, rerr := b.rawFileURL(fb, dataID)
	if rerr != nil {
		return "", "", rerr
	}
	fixed, rerr := b.applyLegacyPrefix(raw, dataID)
	if rerr != nil {
		return "", "", rerr
	}
	return fixed, referer, nil
}

// primaryIsDead reports whether the primary endpoint is eliminated. The
// elimination lifts by itself after primaryRetryAfter: the primary is tried
// again, and if it is still unreachable markPrimaryDead eliminates it for
// another while.
func (b *bunkr) primaryIsDead() bool {
	if b.fallbackAPI == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.primaryDeadAt.IsZero() {
		return false
	}
	if b.now().Sub(b.primaryDeadAt) >= primaryRetryAfter {
		b.primaryDeadAt = time.Time{}
		b.cfg.Logln("bunkr: retrying endpoint %s", b.apiEndpoint)
		return false
	}
	return true
}

// callAPI sends a single POST to the download API.
func (b *bunkr) callAPI(ctx context.Context, endpoint, dataID, referer string) (bunkrAPIResponse, error) {
	var zero bunkrAPIResponse
	if endpoint == "" {
		return zero, Errorf(LayerItemPage, dataID, "no API endpoint defined")
	}

	payload, err := json.Marshal(map[string]string{"id": dataID})
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return zero, Errorf(LayerItemPage, endpoint, "could not build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", referer)
	req.Header.Set("Origin", b.dlOrigin)
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return zero, &LayerError{Layer: LayerItemPage, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusBadRequest {
		// gallery-dl interprets this as "album deleted".
		return zero, Errorf(LayerItemPage, dataID, "API 400: the album or file may have been deleted")
	}
	if resp.StatusCode != http.StatusOK {
		return zero, Errorf(LayerItemPage, endpoint, "API HTTP %s", resp.Status)
	}

	var data bunkrAPIResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return zero, Errorf(LayerItemPage, endpoint, "API response is not JSON: %v", err)
	}
	return data, nil
}

// markPrimaryDead decides whether an error on the primary endpoint requires
// the fallback and, if so, eliminates the endpoint for primaryRetryAfter.
//
// MEASUREMENT 2026-09-10: on this network dl.bunkr.cr is hijacked at the DNS
// level (195.175.254.2 = the ISP's block page, nothing answers on 443).
func (b *bunkr) markPrimaryDead(err error) bool {
	if b.fallbackAPI == "" {
		return false
	}
	// Only CONNECTION errors trigger the fallback; an answer the server gave
	// (400, 404, 500) isn't fixed by the fallback.
	if _, ok := burnReason(err); !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.primaryDeadAt.IsZero() {
		b.primaryDeadAt = b.now()
		b.cfg.Logln("bunkr: endpoint %s unreachable (%v), using the fallback endpoint for %s: %s",
			b.apiEndpoint, unwrapURLError(err), FormatWait(primaryRetryAfter), b.fallbackAPI)
	}
	return true
}

// applyLegacyPrefix adds the storage prefix to the path given by the old endpoint.
//
// For the same file the old endpoint says ".../file.m4v" while the current
// one says ".../storage/media/file.m4v". The prefix was verified by
// MEASUREMENT: on 2026-09-10 it was signed and got 206 on three different
// CDN nodes across two albums. It is still a GUESS; when the current endpoint
// is reachable it tells the real path, so the prefix is only used on the
// fallback path.
func (b *bunkr) applyLegacyPrefix(raw, dataID string) (string, error) {
	if b.legacyPrefix == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", Errorf(LayerItemPage, dataID, "could not parse the old endpoint's URL: %v", err)
	}
	if strings.HasPrefix(u.Path, b.legacyPrefix+"/") {
		return raw, nil
	}
	u.Path = b.legacyPrefix + u.Path
	u.RawPath = ""
	return u.String(), nil
}

// PrepareURL signs the URL right as the download starts. site.URLPreparer.
//
// Because it is called again on every attempt, an expired token refreshes
// itself: there is no need to get a 403 and resolve the item from scratch.
func (b *bunkr) PrepareURL(ctx context.Context, rawURL string) (string, error) {
	return b.signURL(ctx, rawURL)
}

// rawFileURL builds the raw URL to be signed from the API response.
func (b *bunkr) rawFileURL(data bunkrAPIResponse, dataID string) (string, error) {
	if data.MediaFiles != "" && data.Path != "" {
		// Base and path are NOT concatenated and then parsed. If the file name
		// contains '#' or '?', url.Parse takes them for a fragment/query and
		// CUTS the path: "track #3.mp4" -> Path="/…/track ", Fragment="3.mp4".
		// Then the signature is taken for the wrong path and the CDN returns
		// 403 — the hardest error class to diagnose, because it looks like
		// "the signature expired".
		//
		// Parsing is applied only to the base; the path is assigned as a FIELD
		// and String() does the escaping. This also makes it impossible for a
		// path like "@evil.tld/x" to change the host during concatenation.
		u, err := url.Parse(strings.TrimRight(data.MediaFiles, "/"))
		if err != nil {
			return "", Errorf(LayerItemPage, dataID, "could not parse the API URL: %v", err)
		}
		if u.Scheme != "https" || u.Host == "" {
			return "", Errorf(LayerItemPage, dataID,
				"API returned an unexpected download base: %q", data.MediaFiles)
		}
		p := data.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u.Path = p
		u.RawPath = "" // let the escaping be derived from Path again

		// n is the original name the CDN uses in Content-Disposition. The site
		// itself adds it BEFORE signing too; the signature only looks at the
		// path, so the order doesn't change the result.
		if data.Original != "" {
			q := u.Query()
			q.Set("n", data.Original)
			u.RawQuery = q.Encode()
		}
		return u.String(), nil
	}

	if data.URL == "" {
		return "", Errorf(LayerItemPage, dataID, "API gave neither mediafiles/path nor url")
	}
	if !data.Encrypted {
		return data.URL, nil
	}
	key := b.xorPrefix + strconv.FormatInt(data.Timestamp/3600, 10)
	dec, err := decryptXOR(data.URL, []byte(key))
	if err != nil {
		return "", Errorf(LayerItemPage, dataID, "could not decrypt the URL: %v", err)
	}
	return dec, nil
}

// signURL signs the CDN URL with a token from the signing service.
//
// Why a separate service: the CDN rejects an unsigned GET without looking at
// the file. 2026-09-10 measurement: for an existing file and a MADE-UP file
// name the response is byte-for-byte identical (403, same body, same
// headers). So looking at a 403 and saying "the file was deleted" would be WRONG.
//
// The token is time-limited (the ex field). When it expires the downloader
// re-resolves the item with ResolveOne; that is why Item.SourcePage is mandatory.
func (b *bunkr) signURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", Errorf(LayerCDN, rawURL, "could not parse the URL: %v", err)
	}

	// The service gets the DECODED path: the site's JS applies
	// decodeURIComponent and re-encodes with encodeURIComponent. u.Path is
	// already decoded, and QueryEscape encodes everything including "/".
	endpoint := b.signEndpoint + "?path=" + url.QueryEscape(u.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", Errorf(LayerCDN, endpoint, "could not build request: %v", err)
	}
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return "", &LayerError{Layer: LayerCDN, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	b.cfg.Recordln("sign.json", redactToken(body))

	if resp.StatusCode != http.StatusOK {
		// A challenge is classified SEPARATELY: saying "signing service 403"
		// would send the user to the config, while the problem is Cloudflare.
		if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
			return "", &LayerError{
				Layer:    LayerChallenge,
				Err:      errors.New("the signing service returned a Cloudflare challenge"),
				Evidence: endpoint,
			}
		}
		return "", Errorf(LayerCDN, endpoint, "signing service HTTP %s", resp.Status)
	}
	var sig bunkrSignResponse
	if err := json.Unmarshal(body, &sig); err != nil {
		return "", Errorf(LayerCDN, endpoint, "signing response is not JSON: %v", err)
	}
	if sig.Token == "" {
		return "", Errorf(LayerCDN, endpoint, "no token in the signing response")
	}
	// ex is CHECKED too: if it is missing 0 is written, the CDN returns a
	// plain 403 and ClassifyStatus counts it as "signature expired". Result:
	// the same broken URL is built once more and retried, and the user sees
	// the wrong error.
	if sig.Ex <= 0 {
		return "", Errorf(LayerCDN, endpoint, "no valid ex in the signing response")
	}

	q := u.Query()
	q.Set("token", sig.Token)
	q.Set("ex", strconv.FormatInt(sig.Ex, 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// decryptXOR decodes base64 data and XORs it with the key.
//
// The key is time-based (timestamp/3600), so the decrypted URL is valid in an
// hourly window. This is bunkr's reason for Item.SourcePage being mandatory:
// a resume the next day gets 403 on the old URL and the item has to be
// re-resolved.
func decryptXOR(b64 string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("empty key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("could not decode base64: %w", err)
	}
	out := make([]byte, len(raw))
	for i := range raw {
		out[i] = raw[i] ^ key[i%len(key)]
	}
	return string(out), nil
}
