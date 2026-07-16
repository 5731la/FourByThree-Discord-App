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

// inlineEventsRe matches quoted inline event handlers like onclick="..." or onclick='...'
// which Discord's CSP blocks. We rewrite them to data-* attributes and re-bind dynamically.
// NOTE: unquoted values (e.g. onclick=()=>fn()) are intentionally left alone because the
// '>' in '=>' is indistinguishable from an HTML close-tag, so regex rewriting corrupts the JS.
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

// activeGames maps a Discord user ID to the interaction session
type GameSession struct {
	AppID      string
	Token      string
	PuzzleLink string // optional hash fragment from a custom puzzle link
	GameType   string // "4x3" or "smush"
}

var activeGames = struct {
	sync.RWMutex
	m map[string]GameSession
}{m: make(map[string]GameSession)}

// pendingPuzzles maps a Discord channel_id to a puzzle link set by a slash command.
var pendingPuzzles = struct {
	sync.RWMutex
	m map[string]string
}{m: make(map[string]string)}

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

func createFollowupMessage(appID, token, userID, textResult, puzzleLink string, imgBlob []byte) error {
	url := fmt.Sprintf("https://discord.com/api/v10/webhooks/%s/%s", appID, token)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("files[0]", "result.png")
	if err != nil {
		return err
	}
	part.Write(imgBlob)

	payloadStruct := struct {
		Content     string `json:"content"`
		Attachments []struct {
			ID          int    `json:"id"`
			Description string `json:"description"`
		} `json:"attachments"`
	}{
		Content: func() string {
			s := fmt.Sprintf("<@%s> finished their 4\u00d73 game!", userID)
			if puzzleLink != "" {
				s += fmt.Sprintf(" [View puzzle](%s)", puzzleLink)
			}
			return s
		}(),
		Attachments: []struct {
			ID          int    `json:"id"`
			Description string `json:"description"`
		}{
			{ID: 0, Description: textResult},
		},
	}
	payloadBytes, _ := json.Marshal(payloadStruct)
	_ = writer.WriteField("payload_json", string(payloadBytes))
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
	clientSecret := os.Getenv("DISCORD_CLIENT_SECRET")
	redirectURI := os.Getenv("DISCORD_REDIRECT_URI")
	if redirectURI == "" {
		redirectURI = "https://fourbythreediscord.stellasec.com"
	}

	// discordSDKScript is injected into every HTML page served from hankgreen.com.
	// 1. Initialises the Discord Embedded App SDK if running inside an Activity.
	// 2. Restores inline event handlers (onclick, etc.) that Discord's CSP blocks.
	//    Discord's proxy strips 'unsafe-inline' but keeps 'unsafe-eval', allowing
	//    us to bind the rewritten data-on* attributes using new Function().
	discordSDKScript := fmt.Sprintf(`<script type="module">
const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
let authenticatedUserId = null;

if (inDiscord) {
  const { DiscordSDK } = await import('/static/index.mjs');
  const sdk = new DiscordSDK(%q);
  window.discordSdk = sdk;
  await sdk.ready();
  console.log('[4x3] Discord SDK ready');

  try {
    const { code } = await sdk.commands.authorize({
      client_id: %q,
      response_type: 'code',
      prompt: 'none',
      scope: ['identify']
    });
    
    const tokenResp = await fetch('api/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code })
    });
    const tokenData = await tokenResp.json();
    
    const auth = await sdk.commands.authenticate({
      access_token: tokenData.access_token
    });
    authenticatedUserId = auth.user.id;
    console.log('[4x3] Authenticated user', authenticatedUserId);

    const statusRes = await fetch('api/status?user_id=' + authenticatedUserId);
    if (!statusRes.ok) {
      if (typeof window.toast === 'function') {
        window.toast("Start the game with /play4x3 to auto-share your score!", 5000);
      }
    } else {
      const statusData = await statusRes.json();
      // If this session is for a different game, redirect there immediately.
      // Save auth state in sessionStorage so the target page can skip sdk.ready().
      if (statusData.game_type === 'smush' && !window.location.pathname.includes('smush')) {
        try {
          sessionStorage.setItem('discord_user_id', authenticatedUserId);
          if (window.discordSdk && window.discordSdk.channelId) {
            sessionStorage.setItem('discord_channel_id', window.discordSdk.channelId);
          }
        } catch(e) {}
        
        let basePath = window.location.pathname;
        if (!basePath.endsWith('/')) basePath += '/';
        window.location.replace(basePath + 'smush/' + window.location.search + window.location.hash);
      } else if (statusData.puzzle_link) {
        window.__puzzleLink = statusData.puzzle_link;
        // Extract the hash fragment and navigate the game to it.
        try {
          const hashIdx = statusData.puzzle_link.indexOf('#');
          if (hashIdx !== -1) {
            window.location.hash = statusData.puzzle_link.slice(hashIdx + 1);
          }
        } catch(e) { console.error('[4x3] Failed to apply puzzle hash', e); }
      }
    }
  } catch(e) {
    console.error('[4x3] Auth failed', e);
  }
} else {
  console.log('[4x3] Running outside Discord');
}

// Restore inline event handlers that were rewritten to data-on* by the proxy.
// We set .onclick (etc.) directly on each element rather than using event delegation,
// so handlers work regardless of propagation stopping. A MutationObserver ensures
// dynamically-added elements are also patched.
function _patchHandlers() {
  ['onclick','onkeydown','oninput','onchange','onsubmit','onerror','onload'].forEach(ev => {
    const attr = 'data-' + ev;
    document.querySelectorAll('[' + attr + ']').forEach(el => {
      if (el['__patched_' + ev]) return;
      el['__patched_' + ev] = true;
      const code = el.getAttribute(attr);
      try { el[ev] = new Function('event', code); }
      catch(e) { console.warn('[4x3] Failed to patch', attr, e); }
    });
  });
}
_patchHandlers();
document.addEventListener('DOMContentLoaded', _patchHandlers);
new MutationObserver(_patchHandlers).observe(document.documentElement, { childList: true, subtree: true });

// Auto-post image on finish — guard against showResult not yet defined (loaded async).
const _hookShowResult = () => {
  if (typeof window.showResult !== 'function') { setTimeout(_hookShowResult, 200); return; }
  const _origShowResult = window.showResult;
  window.showResult = function(won, score, fancy, quiet) {
    _origShowResult.apply(this, arguments);
    if (!window.__uploadedResult && inDiscord && authenticatedUserId) {
      window.__uploadedResult = true;
      try {
        window.drawShareCard().toBlob(async blob => {
          if (!blob) return;
          const formData = new FormData();
          formData.append('image', blob, 'result.png');
          formData.append('user_id', authenticatedUserId);
          if (window.discordSdk && window.discordSdk.channelId) {
            formData.append('channel_id', window.discordSdk.channelId);
          }
          try { formData.append('text_result', window.resultDescription()); } catch(err) {}
          if (window.__puzzleLink) { formData.append('puzzle_link', window.__puzzleLink); }
          const res = await fetch('api/result', { method: 'POST', body: formData });
          if (!res.ok) {
            if (typeof window.toast === 'function') {
              window.toast("Start game with /play4x3 to auto-share!", 4000);
            }
          } else {
            if (typeof window.toast === 'function') {
              window.toast("Score shared to chat!", 3000);
            }
          }
        });
      } catch (e) { console.error('Upload failed', e); }
    }
  };
};
_hookShowResult();
</script>`, clientID, clientID)

	// smushSDKScript is injected into every Smush HTML page.
	// Smush uses inline JS with gameOver() as the completion hook,
	// drawShareCard() for the canvas image, and shareText() for the text summary.
	smushSDKScript := fmt.Sprintf(`<script type="module">
const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
let authenticatedUserId = null;

if (inDiscord) {
  // Fast path: the 4x3 landing page already ran sdk.ready() and stored auth in sessionStorage.
  // Discord only fires READY once per iframe lifetime, so we must not call sdk.ready() again.
  try {
    const cached = sessionStorage.getItem('discord_user_id');
    if (cached) {
      authenticatedUserId = cached;
      const channelId = sessionStorage.getItem('discord_channel_id');
      window.discordSdk = { channelId };
      console.log('[smush] Using cached Discord auth, user:', authenticatedUserId);
    }
  } catch(e) {}

  // Slow path: opened directly without going through the 4x3 landing page.
  if (!authenticatedUserId) {
    try {
      const { DiscordSDK } = await import('/static/index.mjs');
      const sdk = new DiscordSDK(%q);
      window.discordSdk = sdk;
      await sdk.ready();
      console.log('[smush] Discord SDK ready');

      const { code } = await sdk.commands.authorize({
        client_id: %q,
        response_type: 'code',
        prompt: 'none',
        scope: ['identify']
      });

      const tokenResp = await fetch('api/token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code })
      });
      const tokenData = await tokenResp.json();

      const auth = await sdk.commands.authenticate({
        access_token: tokenData.access_token
      });
      authenticatedUserId = auth.user.id;
      console.log('[smush] Authenticated user', authenticatedUserId);

      const statusRes = await fetch('api/status?user_id=' + authenticatedUserId);
      if (!statusRes.ok) {
        console.log('[smush] No active game session — start with /playsmush to auto-share');
      }
    } catch(e) {
      console.error('[smush] Auth failed', e);
    }
  }
} else {
  console.log('[smush] Running outside Discord');
}

// Hook gameOver() — called by Smush when the puzzle is completed.
const _smushUpload = async () => {
  if (window.__smushUploaded || !inDiscord || !authenticatedUserId) return;
  window.__smushUploaded = true;
  try {
    const canvas = typeof window.drawShareCard === 'function' ? window.drawShareCard() : null;
    if (!canvas) { console.error('[smush] drawShareCard not available'); return; }
    canvas.toBlob(async blob => {
      if (!blob) return;
      const formData = new FormData();
      formData.append('image', blob, 'result.png');
      formData.append('user_id', authenticatedUserId);
      if (window.discordSdk && window.discordSdk.channelId) {
        formData.append('channel_id', window.discordSdk.channelId);
      }
      try { formData.append('text_result', typeof window.shareText === 'function' ? window.shareText() : ''); } catch(e) {}
      const res = await fetch('api/result', { method: 'POST', body: formData });
      if (res.ok) {
        console.log('[smush] Score shared to chat!');
      } else {
        console.warn('[smush] Share failed — start with /playsmush to enable auto-share');
      }
    });
  } catch(e) { console.error('[smush] Upload failed', e); }
};

// Wrap gameOver so we can intercept it after the game defines it.
// Smush defines gameOver inline, so we poll until it exists then wrap it.
const _smushWrap = () => {
  if (typeof window.gameOver !== 'function') { setTimeout(_smushWrap, 200); return; }
  const _orig = window.gameOver;
  window.gameOver = function(...args) {
    const ret = _orig.apply(this, args);
    _smushUpload();
    return ret;
  };
  console.log('[smush] gameOver hooked');
};
_smushWrap();

// Restore inline event handlers that were rewritten to data-on* by the proxy.
function _patchHandlers() {
  ['onclick','onkeydown','oninput','onchange','onsubmit','onerror','onload'].forEach(ev => {
    const attr = 'data-' + ev;
    document.querySelectorAll('[' + attr + ']').forEach(el => {
      if (el['__patched_' + ev]) return;
      el['__patched_' + ev] = true;
      const code = el.getAttribute(attr);
      try { el[ev] = new Function('event', code); }
      catch(e) { console.warn('[smush] Failed to patch', attr, e); }
    });
  });
}
_patchHandlers();
document.addEventListener('DOMContentLoaded', _patchHandlers);
new MutationObserver(_patchHandlers).observe(document.documentElement, { childList: true, subtree: true });

// Unquoted arrow function onclick=()=>fn() attrs survive the proxy rewrite (the regex can't
// safely rewrite them). Discord's CSP blocks them, but the attribute is still in the DOM.
// Re-bind them via addEventListener using eval-equivalent so they run in global scope.
function _patchArrowOnclicks() {
  document.querySelectorAll('[onclick]').forEach(el => {
    if (el.__arrowPatched) return;
    el.__arrowPatched = true;
    const raw = el.getAttribute('onclick');
    if (!raw) return;
    el.removeAttribute('onclick');
    el.addEventListener('click', function(event) {
      try { (new Function('event', raw)).call(this, event); }
      catch(e) { console.warn('[smush] arrow onclick error:', raw, e); }
    });
  });
}
_patchArrowOnclicks();
document.addEventListener('DOMContentLoaded', _patchArrowOnclicks);
new MutationObserver(_patchArrowOnclicks).observe(document.documentElement, { childList: true, subtree: true });
</script>`, clientID, clientID)

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
			ID            string `json:"id"`
			Type          int    `json:"type"`
			Token         string `json:"token"`
			ApplicationID string `json:"application_id"`
			Data          struct {
				Name     string `json:"name"`
				CustomID string `json:"custom_id"`
				Options  []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"options"`
			} `json:"data"`
			Member *struct {
				User struct {
					ID string `json:"id"`
				} `json:"user"`
			} `json:"member"`
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
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

		if req.Type == 2 && req.Data.Name == "playsmush" { // SMUSH SLASH COMMAND
			userID := ""
			if req.Member != nil {
				userID = req.Member.User.ID
			} else if req.User != nil {
				userID = req.User.ID
			}
			_ = userID

			customID := "launch_smush"
			resp := map[string]interface{}{
				"type": 4,
				"data": map[string]interface{}{
					"content": "Time to play Smush!",
					"components": []map[string]interface{}{
						{
							"type": 1,
							"components": []map[string]interface{}{
								{
									"type":      2,
									"style":     1,
									"label":     "Play Now",
									"custom_id": customID,
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Type == 3 && req.Data.CustomID == "launch_smush" { // SMUSH BUTTON CLICK
			var interactionUserID string
			if req.Member != nil {
				interactionUserID = req.Member.User.ID
			} else if req.User != nil {
				interactionUserID = req.User.ID
			}
			if interactionUserID != "" && req.Token != "" && req.ApplicationID != "" {
				activeGames.Lock()
				activeGames.m["smush:"+interactionUserID] = GameSession{
					AppID:    req.ApplicationID,
					Token:    req.Token,
					GameType: "smush",
				}
				activeGames.Unlock()
			}
			// Also store under bare user ID so the fourbythree status endpoint
			// (which the injected script calls first) can detect game_type and redirect.
			if interactionUserID != "" && req.Token != "" && req.ApplicationID != "" {
				activeGames.Lock()
				activeGames.m[interactionUserID] = GameSession{
					AppID:    req.ApplicationID,
					Token:    req.Token,
					GameType: "smush",
				}
				activeGames.Unlock()
			}
			w.Write([]byte(`{"type": 12}`))
			return
		}

		if req.Type == 2 && req.Data.Name == "play4x3" { // SLASH COMMAND
			// Extract optional custom puzzle link
			var puzzleLink string
			for _, opt := range req.Data.Options {
				if opt.Name == "link" {
					puzzleLink = opt.Value
				}
			}

			// If there's a puzzle link extract just the hash fragment for embedding.
			// Store the full URL for the followup message.
			var content string
			if puzzleLink != "" {
				content = fmt.Sprintf("Time to play a custom 4×3 puzzle! [Open puzzle](%s)", puzzleLink)
			} else {
				content = "Time to play 4×3!"
			}

			// Store the puzzle link keyed by this interaction's unique ID so that
			// each button is independently tied to one specific puzzle.
			customID := "launch_4x3"
			if puzzleLink != "" && req.ID != "" {
				pendingPuzzles.Lock()
				pendingPuzzles.m[req.ID] = puzzleLink
				pendingPuzzles.Unlock()
				customID = "launch_4x3:" + req.ID
			}

			resp := map[string]interface{}{
				"type": 4, // ChannelMessageWithSource
				"data": map[string]interface{}{
					"content": content,
					"components": []map[string]interface{}{
						{
							"type": 1, // ActionRow
							"components": []map[string]interface{}{
								{
									"type":      2, // Button
									"style":     1, // Primary
									"label":     "Play Now",
									"custom_id": customID,
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Type == 3 && strings.HasPrefix(req.Data.CustomID, "launch_4x3") { // BUTTON CLICK
			var interactionUserID string
			if req.Member != nil {
				interactionUserID = req.Member.User.ID
			} else if req.User != nil {
				interactionUserID = req.User.ID
			}

			// Retrieve any pending puzzle link that was set by the slash command.
			// The custom_id is "launch_4x3" for daily puzzles, or
			// "launch_4x3:<interactionID>" for custom puzzles.
			var puzzleLink string
			if parts := strings.SplitN(req.Data.CustomID, ":", 2); len(parts) == 2 {
				pendingPuzzles.RLock()
				puzzleLink = pendingPuzzles.m[parts[1]]
				pendingPuzzles.RUnlock()
			}

			if interactionUserID != "" && req.Token != "" && req.ApplicationID != "" {
				activeGames.Lock()
				activeGames.m[interactionUserID] = GameSession{
					AppID:      req.ApplicationID,
					Token:      req.Token,
					PuzzleLink: puzzleLink,
					GameType:   "4x3",
				}
				activeGames.Unlock()
			}
			w.Write([]byte(`{"type": 12}`))
			return
		}

		w.Write([]byte(`{"type": 4, "data": {"content": "Unknown interaction"}}`))
	})

	mux.HandleFunc("/fourbythree/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		var reqBody struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "bad request", 400)
			return
		}

		data := url.Values{}
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
		data.Set("grant_type", "authorization_code")
		data.Set("code", reqBody.Code)
		data.Set("redirect_uri", redirectURI)

		req, err := http.NewRequest("POST", "https://discord.com/api/v10/oauth2/token", strings.NewReader(data.Encode()))
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "upstream error", 502)
			return
		}
		defer resp.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/fourbythree/api/status", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, "missing user_id", 400)
			return
		}

		activeGames.RLock()
		session := activeGames.m[userID]
		activeGames.RUnlock()

		if session.Token == "" {
			http.Error(w, "no active game found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"puzzle_link": session.PuzzleLink,
			"game_type":   session.GameType,
		})
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

		userID := r.FormValue("user_id")
		textResult := r.FormValue("text_result")
		puzzleLink := r.FormValue("puzzle_link")
		// Fall back to puzzle link stored in session (set at button-click time)
		if puzzleLink == "" {
			activeGames.RLock()
			puzzleLink = activeGames.m[userID].PuzzleLink
			activeGames.RUnlock()
		}
		file, _, err := r.FormFile("image")
		if err != nil || userID == "" {
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
		session := activeGames.m[userID]
		activeGames.RUnlock()

		if session.Token == "" {
			http.Error(w, "no active game found", 404)
			return
		}

		if err := createFollowupMessage(session.AppID, session.Token, userID, textResult, puzzleLink, imgBlob); err != nil {
			log.Printf("Failed to create followup message: %v", err)
			http.Error(w, "failed to post", 500)
			return
		}
		w.WriteHeader(200)
	})

	// --- Smush API routes ---

	// smushProxyModifyResponse wraps the proxy's ModifyResponse to inject the
	// Smush SDK script instead of the 4x3 one for requests under /smush/.
	smushProxy := httputil.NewSingleHostReverseProxy(target)
	smushProxy.Transport = followRedirectsTransport{}
	smushProxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("[smush-proxy] upstream %s %s → %d", resp.Request.Method, resp.Request.URL, resp.StatusCode)
		resp.Header.Del("X-Frame-Options")
		resp.Header.Del("Content-Security-Policy")
		resp.Header.Del("Content-Security-Policy-Report-Only")
		resp.Header.Set("Access-Control-Allow-Origin", "*")
		ct := resp.Header.Get("Content-Type")
		isHTML := resp.StatusCode == http.StatusOK && strings.Contains(ct, "text/html")
		isJS := resp.StatusCode == http.StatusOK && (strings.Contains(ct, "javascript") || strings.Contains(ct, "text/plain"))
		if isHTML || isJS {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			// Rewrite calls to the Cloudflare Worker stats API so they go through our proxy.
			// This avoids CORS/CSP blocks inside Discord's iframe.
			modified := bytes.ReplaceAll(body,
				[]byte("https://fourbythree-stats.hankmt.workers.dev"),
				[]byte("/smush/workerproxy"),
			)
			if isHTML {
				modified = bytes.Replace(modified, []byte("</body>"), []byte(smushSDKScript+"\n</body>"), 1)
				modified = cspMetaRe.ReplaceAll(modified, nil)
				modified = inlineEventsRe.ReplaceAll(modified, []byte(`data-$1=$2`))
			}
			resp.Body = io.NopCloser(bytes.NewReader(modified))
			resp.ContentLength = int64(len(modified))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(modified)))
			resp.Header.Del("Transfer-Encoding")
		}
		return nil
	}

	mux.HandleFunc("/smush/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		var reqBody struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		data := url.Values{}
		data.Set("client_id", clientID)
		data.Set("client_secret", clientSecret)
		data.Set("grant_type", "authorization_code")
		data.Set("code", reqBody.Code)
		data.Set("redirect_uri", redirectURI)
		req, err := http.NewRequest("POST", "https://discord.com/api/v10/oauth2/token", strings.NewReader(data.Encode()))
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "upstream error", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/smush/api/status", func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("user_id")
		if userID == "" {
			http.Error(w, "missing user_id", 400)
			return
		}
		activeGames.RLock()
		session := activeGames.m["smush:"+userID]
		activeGames.RUnlock()
		if session.Token == "" {
			http.Error(w, "no active game found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/smush/api/result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		userID := r.FormValue("user_id")
		textResult := r.FormValue("text_result")
		file, _, err := r.FormFile("image")
		if err != nil || userID == "" {
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
		session := activeGames.m["smush:"+userID]
		activeGames.RUnlock()
		if session.Token == "" {
			http.Error(w, "no active game found", 404)
			return
		}
		content := fmt.Sprintf("<@%s> finished today's Smush!", userID)
		if err := createFollowupMessage(session.AppID, session.Token, userID, textResult, "", imgBlob); err != nil {
			log.Printf("[smush] Failed to post followup: %v (content would have been: %s)", err, content)
			http.Error(w, "failed to post", 500)
			return
		}
		w.WriteHeader(200)
	})

	// /smush/* — proxy Smush game and its assets.
	// Requests arrive as /smush/... or /fourbythree/smush/... (the catch-all
	// prepends /fourbythree for asset requests); strip that before forwarding.
	smushGameHandler := func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		p = strings.TrimPrefix(p, "/fourbythree")
		if !strings.HasPrefix(p, "/smush") {
			p = "/smush" + p
		}
		r.URL.Path = p
		r.URL.RawPath = ""
		r.Host = ""
		log.Printf("[smush-proxy] → https://www.hankgreen.com%s", r.URL.Path)
		smushProxy.ServeHTTP(w, r)
	}
	// workerProxy forwards requests to the Smush Cloudflare Worker stats API.
	workerTarget, _ := url.Parse("https://fourbythree-stats.hankmt.workers.dev")
	workerProxy := httputil.NewSingleHostReverseProxy(workerTarget)
	workerProxy.Transport = followRedirectsTransport{}
	workerProxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("[worker-proxy] upstream %s %s → %d", resp.Request.Method, resp.Request.URL, resp.StatusCode)
		resp.Header.Set("Access-Control-Allow-Origin", "*")
		return nil
	}
	workerProxyHandler := func(w http.ResponseWriter, r *http.Request) {
		for _, pfx := range []string{"/fourbythree/smush/workerproxy", "/smush/workerproxy"} {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, pfx)
			r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, pfx)
		}
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.Host = ""
		log.Printf("[worker-proxy] → https://fourbythree-stats.hankmt.workers.dev%s", r.URL.Path)
		workerProxy.ServeHTTP(w, r)
	}
	// Register before /fourbythree/smush/ so the more-specific pattern wins.
	mux.HandleFunc("/fourbythree/smush/workerproxy/", workerProxyHandler)
	mux.HandleFunc("/fourbythree/smush/workerproxy", workerProxyHandler)
	mux.HandleFunc("/smush/workerproxy/", workerProxyHandler)
	mux.HandleFunc("/smush/workerproxy", workerProxyHandler)

	mux.HandleFunc("/smush/", smushGameHandler)
	mux.HandleFunc("/smush", smushGameHandler)
	// Discord's portal prefixes every path with /fourbythree, so /smush/ arrives as /fourbythree/smush/
	mux.HandleFunc("/fourbythree/smush/", smushGameHandler)
	mux.HandleFunc("/fourbythree/smush", smushGameHandler)

	// Serve the Discord SDK and other static assets from client/static/.
	staticFS, err := fs.Sub(staticFiles, "client/static")
	if err != nil {
		log.Fatalf("static fs error: %v", err)
	}
	mux.Handle("/fourbythree/static/", http.StripPrefix("/fourbythree/static/", http.FileServer(http.FS(staticFS))))
	// Smush also needs the Discord SDK — serve it under /smush/static/ too.
	mux.Handle("/smush/static/", http.StripPrefix("/smush/static/", http.FileServer(http.FS(staticFS))))

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
