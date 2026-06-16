package main

import (
	"bytes"
	"crypto/tls"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// upstreamTransport is used for all requests to hankgreen.com.
//
// HTTP/2 is deliberately disabled. hankgreen.com (Vercel) uses HTTP/2
// multiplexing: all requests share one connection. The Next.js streaming
// HTML response keeps a stream open for 60 s+; if our proxy is slow to
// drain it (due to NGINX backpressure), Go's HTTP/2 stack stops updating
// the connection-level receive window, stalling every other stream on that
// connection — including image requests like fishtrans.png.
//
// Forcing HTTP/1.1 gives each proxied request its own TCP connection so
// one slow stream can never block another.
var upstreamTransport http.RoundTripper = &http.Transport{
	TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// followRedirectsTransport follows HTTP redirects internally so 3xx responses
// from Vercel/hankgreen are never forwarded to the browser.
//
// We use upstreamTransport.RoundTrip directly (not http.Client.Do) so that
// response bodies are streamed immediately with no extra buffering — http.Client
// adds connection-lifecycle overhead that delayed chunked/streaming responses by
// over a minute.
type followRedirectsTransport struct{}

func (followRedirectsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// httputil.ReverseProxy's Director sets RequestURI, which http.Transport
	// rejects on outgoing requests. Clear it before the first hop.
	req.RequestURI = ""

	resp, err := upstreamTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Manually follow up to 10 redirects, chaining RoundTrip calls so the
	// final response body is always a direct stream from the upstream.
	for i := 0; i < 10 && isRedirect(resp.StatusCode); i++ {
		resp.Body.Close()
		loc, err := resp.Location()
		if err != nil {
			break
		}
		log.Printf("[proxy] following redirect → %s", loc)
		next := req.Clone(req.Context())
		next.URL = loc
		next.Host = loc.Host
		next.RequestURI = ""
		resp, err = upstreamTransport.RoundTrip(next)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound,
		http.StatusSeeOther, http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	}
	return false
}

// staticFiles serves the Discord SDK and any other static assets from
// client/static/, avoiding CDN MIME-type and CORS issues.
//
//go:embed all:client/static
var staticFiles embed.FS

func main() {
	clientID := os.Getenv("DISCORD_CLIENT_ID")
	if clientID == "" {
		clientID = "YOUR_CLIENT_ID"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "4343"
	}

	// discordSDKScript is injected into every HTML page served from hankgreen.com.
	// It initialises the Discord Embedded App SDK only when the page is loaded
	// inside a Discord Activity (detected via the frame_id query param).
	discordSDKScript := fmt.Sprintf(`<script type="module">
const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
if (inDiscord) {
  const { DiscordSDK } = await import('/static/index.mjs');
  const sdk = new DiscordSDK(%q);
  await sdk.ready();
  console.log('[4x3] Discord SDK ready');
} else {
  console.log('[4x3] Running outside Discord');
}
</script>`, clientID)

	// Reverse proxy to hankgreen.com.
	// All game paths are forwarded directly — no nested iframe — so that
	// Next.js client-side routing works at the correct path (/fourbythree).
	target, _ := url.Parse("https://www.hankgreen.com")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = followRedirectsTransport{}

	proxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("[proxy] upstream %s %s → %d", resp.Request.Method, resp.Request.URL, resp.StatusCode)

		// Remove headers that would prevent embedding.
		resp.Header.Del("X-Frame-Options")
		resp.Header.Del("Content-Security-Policy")
		resp.Header.Del("Content-Security-Policy-Report-Only")
		resp.Header.Set("Access-Control-Allow-Origin", "*")

		// Inject the Discord SDK initialisation script into HTML responses.
		// This replaces the old nested-iframe approach: the game now runs
		// directly in the Discord Activity iframe at the correct URL path.
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode == http.StatusOK && strings.Contains(ct, "text/html") {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			modified := bytes.Replace(body, []byte("</body>"), []byte(discordSDKScript+"\n</body>"), 1)
			resp.Body = io.NopCloser(bytes.NewReader(modified))
			resp.ContentLength = int64(len(modified))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(modified)))
			// Remove transfer-encoding so the fixed Content-Length is authoritative.
			resp.Header.Del("Transfer-Encoding")
		}

		return nil
	}

	mux := http.NewServeMux()

	// Serve the Discord SDK and other static assets from client/static/.
	staticFS, err := fs.Sub(staticFiles, "client/static")
	if err != nil {
		log.Fatalf("static fs error: %v", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// proxyHandler strips a path prefix then forwards to hankgreen.com.
	proxyHandler := func(stripPrefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, stripPrefix)
			r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, stripPrefix)
			r.Host = ""
			log.Printf("[proxy] → https://www.hankgreen.com%s", r.URL.Path)
			proxy.ServeHTTP(w, r)
		}
	}

	// /fourbythree/* — the game itself and its assets (puzzles.json, images, etc.)
	mux.HandleFunc("/fourbythree/", proxyHandler(""))
	mux.HandleFunc("/fourbythree", proxyHandler(""))

	// /_next/* — Next.js static bundles (JS, CSS) referenced by absolute path in the HTML.
	mux.HandleFunc("/_next/", proxyHandler(""))

	// / — redirect root to the game's canonical path.
	// Catch-all: the game JS fetches assets with root-relative paths like
	// /puzzles.json and /fishtrans.png. In Discord's proxy these arrive here
	// without the /fourbythree prefix, so we prepend it before forwarding.
	// Explicit handlers above (/fourbythree/, /_next/, /static/) take priority.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/fourbythree", http.StatusFound)
			return
		}
		r.URL.Path = "/fourbythree" + r.URL.Path
		if r.URL.RawPath != "" {
			r.URL.RawPath = "/fourbythree" + r.URL.RawPath
		}
		r.Host = ""
		log.Printf("[proxy] → https://www.hankgreen.com%s (prepended /fourbythree)", r.URL.Path)
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + port
	log.Printf("FourByThree Discord App listening on http://localhost%s", addr)
	log.Printf("Discord Client ID: %s", clientID)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
