// Unified SDK script — injected into game iframes.
// Template placeholder: {{.ClientID}} is replaced at startup.

const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
const isSmush = window.location.pathname.includes('/smush/');
const gameTag = isSmush ? 'smush' : '4x3';
let authenticatedUserId = null;

if (inDiscord) {
  // Fast path: parent passed auth via URL params.
  const urlParams = new URLSearchParams(window.location.search);
  const uid = urlParams.get('_uid');
  const cid = urlParams.get('_cid');
  if (uid && cid) {
    authenticatedUserId = uid;
    window.discordSdk = { channelId: cid };
    console.log('[' + gameTag + '] Using URL param auth, user:', authenticatedUserId);
  }

  // Slow path: direct load without parent.
  if (!authenticatedUserId) {
    try {
      const { DiscordSDK } = await import('/static/index.mjs');
      const sdk = new DiscordSDK("{{.ClientID}}");
      window.discordSdk = sdk;
      await sdk.ready();
      console.log('[' + gameTag + '] Discord SDK ready');

      const { code } = await sdk.commands.authorize({
        client_id: "{{.ClientID}}",
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
      console.log('[' + gameTag + '] Authenticated user', authenticatedUserId);
    } catch(e) {
      console.error('[' + gameTag + '] Auth failed', e);
    }
  }
} else {
  console.log('[' + gameTag + '] Running outside Discord');
}

// Restore inline event handlers that were rewritten to data-on* by the proxy.
function _patchHandlers() {
  ['onclick','onkeydown','oninput','onchange','onsubmit','onerror','onload'].forEach(ev => {
    const attr = 'data-' + ev;
    document.querySelectorAll('[' + attr + ']').forEach(el => {
      if (el['__patched_' + ev]) return;
      el['__patched_' + ev] = true;
      const code = el.getAttribute(attr);
      try { el[ev] = new Function('event', code); }
      catch(e) { console.warn('[' + gameTag + '] Failed to patch', attr, e); }
    });
  });
}
_patchHandlers();
document.addEventListener('DOMContentLoaded', _patchHandlers);
new MutationObserver(_patchHandlers).observe(document.documentElement, { childList: true, subtree: true });

// Unquoted arrow function onclick=()=>fn() attrs survive the proxy rewrite.
function _patchArrowOnclicks() {
  document.querySelectorAll('[onclick]').forEach(el => {
    if (el.__arrowPatched) return;
    el.__arrowPatched = true;
    const raw = el.getAttribute('onclick');
    if (!raw) return;
    el.removeAttribute('onclick');
    el.addEventListener('click', function(event) {
      try { (new Function('event', raw)).call(this, event); }
      catch(e) { console.warn('[' + gameTag + '] arrow onclick error:', raw, e); }
    });
  });
}
_patchArrowOnclicks();
document.addEventListener('DOMContentLoaded', _patchArrowOnclicks);
new MutationObserver(_patchArrowOnclicks).observe(document.documentElement, { childList: true, subtree: true });

// Game-specific completion hook.
const _uploadScore = async () => {
  const todayKey = gameTag + '_uploaded_' + new Date().toDateString();
  if (window['__' + gameTag + 'Uploaded'] || !inDiscord || !authenticatedUserId || localStorage.getItem(todayKey)) return;
  window['__' + gameTag + 'Uploaded'] = true;
  localStorage.setItem(todayKey, '1');
  try {
    const canvas = typeof window.drawShareCard === 'function' ? window.drawShareCard() : null;
    if (!canvas) { console.error('[' + gameTag + '] drawShareCard not available'); return; }
    canvas.toBlob(async blob => {
      if (!blob) return;
      const formData = new FormData();
      formData.append('image', blob, 'result.png');
      formData.append('user_id', authenticatedUserId);
      if (window.discordSdk && window.discordSdk.channelId) {
        formData.append('channel_id', window.discordSdk.channelId);
      }
      try {
        formData.append('text_result', typeof window.shareText === 'function' ? window.shareText() : (typeof window.resultDescription === 'function' ? window.resultDescription() : ''));
      } catch(e) {}
      const res = await fetch('api/result', { method: 'POST', body: formData });
      if (res.ok) {
        console.log('[' + gameTag + '] Score shared to chat!');
      } else {
        console.warn('[' + gameTag + '] Share failed');
      }
    });
  } catch(e) { console.error('[' + gameTag + '] Upload failed', e); }
};

if (isSmush) {
  // Smush: observe #finalScore in #mbody (game over modal container).
  const _smushObserver = new MutationObserver(() => {
    if (document.getElementById('finalScore')) {
      _uploadScore();
      _smushObserver.disconnect();
    }
  });
  const mbody = document.getElementById('mbody');
  if (mbody) {
    _smushObserver.observe(mbody, { childList: true, subtree: true });
    if (document.getElementById('finalScore')) _uploadScore();
  } else {
    _smushObserver.observe(document.body, { childList: true, subtree: true });
  }
  console.log('[' + gameTag + '] gameOver observer installed');
} else {
  // 4x3: hook showResult — only upload when won=true (game actually finished).
  const _hookShowResult = () => {
    if (typeof window.showResult !== 'function') { setTimeout(_hookShowResult, 200); return; }
    const _origShowResult = window.showResult;
    window.showResult = function(won, score, fancy, quiet) {
      _origShowResult.apply(this, arguments);
      if (!won || window.__uploadedResult || !inDiscord || !authenticatedUserId) return;
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
          if (res.ok) {
            if (typeof window.toast === 'function') window.toast("Score shared to chat!", 3000);
          } else {
            if (typeof window.toast === 'function') window.toast("Start game with /play4x3 to auto-share!", 4000);
          }
        });
      } catch (e) { console.error('Upload failed', e); }
    };
  };
  _hookShowResult();
}
