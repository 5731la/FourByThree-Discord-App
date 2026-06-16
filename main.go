package main

import (
	"crypto/tls"
	"embed"
	"fmt"
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
// We use http.DefaultTransport.RoundTrip directly (not http.Client.Do) so that
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

// indexHTML is embedded at compile time from client/index.html.
// The file lives on disk for easy editing; the binary stays self-contained.
//
//go:embed client/index.html
var indexHTML []byte

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

	// Reverse proxy to hankgreen.com for local development.
	// In Discord, the Dev Portal URL Mapping handles /hankgreen → www.hankgreen.com.
	target, _ := url.Parse("https://www.hankgreen.com")
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Follow redirects inside the proxy so 3xx responses from Vercel/hankgreen
	// are never forwarded to the browser.
	proxy.Transport = followRedirectsTransport{}

	proxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("[proxy] upstream %s %s → %d", resp.Request.Method, resp.Request.URL, resp.StatusCode)
		// Remove headers that would prevent the page from being embedded.
		resp.Header.Del("X-Frame-Options")
		resp.Header.Del("Content-Security-Policy")
		resp.Header.Del("Content-Security-Policy-Report-Only")
		// Ensure CORS is open for local dev.
		resp.Header.Set("Access-Control-Allow-Origin", "*")
		return nil
	}

	mux := http.NewServeMux()

	// Wrapper page — the Discord Activity entry point.
	// Injects DISCORD_CLIENT_ID into the page via a small inline script before </head>.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Inject the client ID as a global JS variable so index.html can use it
		// without needing a build step or template engine.
		injection := fmt.Sprintf(
			`<script>window.__DISCORD_CLIENT_ID__ = %q;</script>`,
			clientID,
		)
		html := strings.Replace(string(indexHTML), "</head>", injection+"\n</head>", 1)
		fmt.Fprint(w, html)
	})

	// Serve the Discord SDK and other static assets from client/static/.
	// Using a local copy avoids CDN CORS restrictions and wrong MIME types.
	staticFS, err := fs.Sub(staticFiles, "client/static")
	if err != nil {
		log.Fatalf("static fs error: %v", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Local dev proxy: /hankgreen/* → https://www.hankgreen.com/*
	// Strips the /hankgreen prefix before forwarding.
	mux.HandleFunc("/hankgreen/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/hankgreen")
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/hankgreen")
		// IMPORTANT: clear r.Host so the reverse proxy uses the target host
		// (www.hankgreen.com) as the Host header rather than forwarding the
		// client's host (e.g. 192.168.100.138:3000). Vercel rejects unknown
		// hosts with a 308 redirect to its canonical deployment URL.
		r.Host = ""
		log.Printf("[proxy] → https://www.hankgreen.com%s", r.URL.Path)
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + port
	log.Printf("FourByThree Discord App listening on http://localhost%s", addr)
	log.Printf("Discord Client ID: %s", clientID)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
