package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// cspMetaRe matches a <meta http-equiv="Content-Security-Policy" ...> tag.
// Next.js 13+ embeds the CSP in both a response header and this meta tag;
// we strip the header in ModifyResponse but must also remove the meta tag
// so its nonce-based policy does not block inline event handlers in the game.
var cspMetaRe = regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']?Content-Security-Policy["']?[^>]*/?>`)

// inlineEventsRe matches inline event handlers like onclick="..." which Discord's
// CSP blocks because it removes the 'unsafe-inline' keyword. We rewrite them to
// data-onclick="..." and bind them dynamically. This regex matches both single
// and double quotes to catch handlers inside dynamically injected HTML strings.
var inlineEventsRe = regexp.MustCompile(`(?i)\b(on(?:click|keydown|input|error|load|change|submit))=(["'][^"']*["'])`)

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

	// Force uncompressed responses from hankgreen.com so our ModifyResponse
	// can process HTML bodies (inject SDK script, strip CSP meta tag) as
	// plain text. Discord's proxy sends Accept-Encoding: gzip, which without
	// this would cause Vercel to return a gzipped response that bypasses our
	// regex rewrites.
	req.Header.Set("Accept-Encoding", "identity")

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

// activeGames maps a Discord channel ID to the interaction session
type GameSession struct {
	AppID string
	Token string
}

var activeGames = struct {
	sync.RWMutex
	m map[string]GameSession
}{m: make(map[string]GameSession)}

func verifyInteraction(r *http.Request, pubKey ed25519.PublicKey) ([]byte, bool) {
	sigHex := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	if sigHex == "" || ts == "" {
		return nil, false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	msg := []byte(ts)
	msg = append(msg, body...)
	return body, ed25519.Verify(pubKey, msg, sig)
}

func createFollowupMessage(appID, token string, imgBlob []byte) error {
	url := fmt.Sprintf("https://discord.com/api/v10/webhooks/%s/%s", appID, token)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("files[0]", "result.png")
	if err != nil {
		return err
	}
	part.Write(imgBlob)

	payload := `{"content": "A player finished their 4×3 game!"}`
	_ = writer.WriteField("payload_json", payload)
	writer.Close()

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord API error: %d %s", resp.StatusCode, string(b))
	}
	return nil
}

func main() {
	clientID := os.Getenv("DISCORD_CLIENT_ID")
	if clientID == "" {
		clientID = "YOUR_CLIENT_ID"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "4343"
	}

	pubKeyHex := os.Getenv("DISCORD_PUBLIC_KEY")
	var pubKey ed25519.PublicKey
	if pubKeyHex != "" {
		b, _ := hex.DecodeString(pubKeyHex)
		pubKey = ed25519.PublicKey(b)
	}

	// discordSDKScript is injected into every HTML page served from hankgreen.com.
	// 1. Initialises the Discord Embedded App SDK if running inside an Activity.
	// 2. Restores inline event handlers (onclick, etc.) that Discord's CSP blocks.
	//    Discord's proxy strips 'unsafe-inline' but keeps 'unsafe-eval', allowing
	//    us to bind the rewritten data-on* attributes using new Function().
	discordSDKScript := fmt.Sprintf(`<script type="module">
const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
if (inDiscord) {
  const { DiscordSDK } = await import('/static/index.mjs');
  const sdk = new DiscordSDK(%q);
  window.discordSdk = sdk;
  await sdk.ready();
  console.log('[4x3] Discord SDK ready');
} else {
  console.log('[4x3] Running outside Discord');
}

['click', 'keydown', 'input', 'change', 'submit'].forEach(evt => {
  document.addEventListener(evt, function(event) {
    let target = event.target;
    while (target && target !== document) {
      if (target.hasAttribute && target.hasAttribute('data-on' + evt)) {
        const code = target.getAttribute('data-on' + evt);
        const res = new Function('event', code).call(target, event);
        if (res === false) {
          event.preventDefault();
        }
      }
      target = target.parentNode;
    }
  });
});

['error', 'load'].forEach(evt => {
  document.querySelectorAll('[data-on' + evt + ']').forEach(el => {
    const code = el.getAttribute('data-on' + evt);
    el.addEventListener(evt, function(event) {
      new Function('event', code).call(this, event);
    });
  });
});

// Auto-post image on finish
const originalShowEnd = openModal;
if (typeof originalShowEnd === 'function') {
  openModal = function(fancy) {
    originalShowEnd(fancy);
    if (!window.__uploadedResult && inDiscord && window.discordSdk && window.discordSdk.channelId) {
      window.__uploadedResult = true;
      try {
        window.drawShareCard().toBlob(async blob => {
          if (!blob) return;
          const formData = new FormData();
          formData.append('image', blob, 'result.png');
          formData.append('channel_id', window.discordSdk.channelId);
          await fetch('api/result', { method: 'POST', body: formData });
        });
      } catch (e) { console.error('Upload failed', e); }
    }
  };
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
			// Also strip any CSP meta tag — Next.js embeds the policy in the
			// HTML body as well as the header, and its nonce blocks the game's
			// own inline event handlers when served inside Discord's iframe.
			modified = cspMetaRe.ReplaceAll(modified, nil)

			// Rewrite inline event handlers (onclick="..." -> data-onclick="...")
			// so they bypass Discord's strict CSP blocking 'unsafe-inline'.
			modified = inlineEventsRe.ReplaceAll(modified, []byte(`data-$1=$2`))

			resp.Body = io.NopCloser(bytes.NewReader(modified))
			resp.ContentLength = int64(len(modified))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(modified)))
			// Remove transfer-encoding so the fixed Content-Length is authoritative.
			resp.Header.Del("Transfer-Encoding")
		}

		return nil
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/discord/interactions", func(w http.ResponseWriter, r *http.Request) {
		if pubKey == nil {
			http.Error(w, "Bot not configured", 500)
			return
		}
		body, ok := verifyInteraction(r, pubKey)
		if !ok {
			http.Error(w, "invalid request signature", 401)
			return
		}

		var req struct {
			Type          int    `json:"type"`
			Token         string `json:"token"`
			ApplicationID string `json:"application_id"`
			Data          struct {
				Name     string `json:"name"`
				CustomID string `json:"custom_id"`
			} `json:"data"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if req.Type == 1 { // PING
			w.Write([]byte(`{"type": 1}`))
			return
		}

		if req.Type == 2 && req.Data.Name == "play4x3" { // SLASH COMMAND
			resp := map[string]interface{}{
				"type": 4, // ChannelMessageWithSource
				"data": map[string]interface{}{
					"content": "Time to play 4×3!",
					"components": []map[string]interface{}{
						{
							"type": 1, // ActionRow
							"components": []map[string]interface{}{
								{
									"type":      2, // Button
									"style":     1, // Primary
									"label":     "Play Now",
									"custom_id": "launch_4x3",
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Type == 3 && req.Data.CustomID == "launch_4x3" { // BUTTON CLICK
			if req.ChannelID != "" && req.Token != "" && req.ApplicationID != "" {
				activeGames.Lock()
				activeGames.m[req.ChannelID] = GameSession{
					AppID: req.ApplicationID,
					Token: req.Token,
				}
				activeGames.Unlock()
			}
			w.Write([]byte(`{"type": 12}`))
			return
		}

		w.Write([]byte(`{"type": 4, "data": {"content": "Unknown interaction"}}`))
	})

	mux.HandleFunc("/fourbythree/api/result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}

		err := r.ParseMultipartForm(10 << 20) // 10 MB
		if err != nil {
			http.Error(w, "bad form", 400)
			return
		}

		channelID := r.FormValue("channel_id")
		file, _, err := r.FormFile("image")
		if err != nil || channelID == "" {
			http.Error(w, "missing fields", 400)
			return
		}
		defer file.Close()

		imgBlob, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "read error", 500)
			return
		}

		activeGames.RLock()
		session := activeGames.m[channelID]
		activeGames.RUnlock()

		if session.Token == "" {
			http.Error(w, "no active game found", 404)
			return
		}

		go func() {
			if err := createFollowupMessage(session.AppID, session.Token, imgBlob); err != nil {
				log.Printf("Failed to create followup message: %v", err)
			}
		}()
		w.WriteHeader(200)
	})

	// Serve the Discord SDK and other static assets from client/static/.
	staticFS, err := fs.Sub(staticFiles, "client/static")
	if err != nil {
		log.Fatalf("static fs error: %v", err)
	}
	mux.Handle("/fourbythree/static/", http.StripPrefix("/fourbythree/static/", http.FileServer(http.FS(staticFS))))

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
