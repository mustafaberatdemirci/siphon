// Spike: varsayilan Go net/http Transport ile bunkr/pixeldrain erisimi olculur.
// Amac: TLS parmak izi nedeniyle Cloudflare challenge aliyor muyuz?
// curl ile olculemez: curl'un parmak izi Go'nunkinden farklidir.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	urls := os.Args[1:]
	if len(urls) == 0 {
		fmt.Println("kullanim: go run main.go <url> [url...]")
		os.Exit(2)
	}

	for _, u := range urls {
		// Transport varsayilan kalir (TLS parmak izi olculen sey bu).
		// Sadece redirect zincirini gorunur kiliyoruz.
		var chain []string
		c := &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				chain = append(chain, fmt.Sprintf("%d-> %s", len(via), r.URL.String()))
				if len(via) >= 10 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}

		fmt.Printf("\n================ %s ================\n", u)
		resp, err := c.Get(u)
		if err != nil {
			fmt.Printf("HATA: %v\n", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		fmt.Printf("STATUS  : %s\n", resp.Status)
		fmt.Printf("SERVER  : %s\n", resp.Header.Get("Server"))
		fmt.Printf("CF-RAY  : %s\n", resp.Header.Get("CF-Ray"))
		fmt.Printf("CF-MIT  : %s\n", resp.Header.Get("CF-Mitigated"))
		fmt.Printf("TYPE    : %s\n", resp.Header.Get("Content-Type"))
		if len(chain) > 0 {
			fmt.Printf("REDIRECT: %s\n", strings.Join(chain, " | "))
		}

		s := string(body)
		low := strings.ToLower(s)
		var marks []string
		for _, m := range []string{"just a moment", "cf-challenge", "challenge-platform",
			"cf_chl", "turnstile", "enable javascript and cookies", "attention required"} {
			if strings.Contains(low, m) {
				marks = append(marks, m)
			}
		}
		if len(marks) > 0 {
			fmt.Printf("CHALLENGE MARKER: %s\n", strings.Join(marks, ", "))
		} else {
			fmt.Printf("CHALLENGE MARKER: yok\n")
		}

		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > 300 {
			s = s[:300]
		}
		fmt.Printf("BODY[0:300]: %s\n", s)
	}
}
