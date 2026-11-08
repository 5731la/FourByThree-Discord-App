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
      const { DiscordSDK } = await import('/static/index.mjs?{{.CacheBust}}');
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

// === Core share function — posts a score image + text to Discord via the Go proxy. ===
const _postScoreToDiscord = async (imageBlob, text) => {
  if (!authenticatedUserId) { console.warn('[' + gameTag + '] Not authenticated'); return false; }
  if (!imageBlob) { console.error('[' + gameTag + '] No score image available'); return false; }
  try {
    const formData = new FormData();
    formData.append('image', imageBlob, 'result.png');
    formData.append('user_id', authenticatedUserId);
    if (window.discordSdk && window.discordSdk.channelId) {
      formData.append('channel_id', window.discordSdk.channelId);
    }
    formData.append('text_result', text || '');
    if (window.__puzzleLink) { formData.append('puzzle_link', window.__puzzleLink); }
    const _cheatedDate = getCheatedDate();
    if (_cheatedDate) { formData.append('cheated', _cheatedDate); }
    const res = await fetch('api/result', { method: 'POST', body: formData });
    if (res.ok) { console.log('[' + gameTag + '] Score shared to chat!'); }
    else { console.warn('[' + gameTag + '] Share failed'); }
    return res.ok;
  } catch(e) { console.error('[' + gameTag + '] Upload failed', e); return false; }
};

// === Auto-upload with daily guard (called by the game-completion observers) ===
const _uploadScore = async (imageBlob, text) => {
  const todayKey = gameTag + '_uploaded_' + new Date().toDateString();
  if (window['__' + gameTag + 'Uploaded'] || !inDiscord || !authenticatedUserId || localStorage.getItem(todayKey)) return;
  window['__' + gameTag + 'Uploaded'] = true;
  localStorage.setItem(todayKey, '1');
  await _postScoreToDiscord(imageBlob, text);
};

// === DOM capture helpers (new Vite/React build: no window globals; the result UI
// renders the share card as a data: URL <img> and the share text in known nodes) ===
const _sleep = ms => new Promise(r => setTimeout(r, ms));
const _cardBlobFromImg = async img => (await fetch(img.currentSrc || img.src)).blob();

// === Cheating flag ===
// Resetting the board (smush: 5 quick logo clicks; 4x3: the secret key combo
// or the hidden test menu) is not a legitimate play. When it happens we tag
// today's game as cheated under a key mirroring the daily upload guard above,
// after warning the user. _postScoreToDiscord sends the tag along, and the Go
// proxy then posts the score to Discord as "<@user> (CHEATER)".
function getCheatedDate() {
  const key = gameTag + '_cheated_' + new Date().toDateString();
  try { return localStorage.getItem(key) || ''; } catch (e) { return ''; }
}
function markCheated() {
  const key = gameTag + '_cheated_' + new Date().toDateString();
  try { localStorage.setItem(key, new Date().toDateString()); } catch (e) {}
}
const _resetWarning =
  "Cheating warning\n\n" +
  "Resetting the board means today's " + (isSmush ? 'Smush' : '4\u00d73') +
  " run is not a legitimate play.\n\n" +
  "If you send your score to Discord chat, it will be posted as:\n\n" +
  "    @you (CHEATER)\n\n" +
  "Reset the board anyway?";
function _askInPage(message) {
  // Discord's activity iframe is sandboxed without allow-modals: native
  // dialogs (confirm/alert) are silently ignored and log an error, so the
  // warning is always shown as an in-page modal.
  return new Promise((resolve) => {
    const overlay = document.createElement('div');
    overlay.id = '__fb3Confirm';
    overlay.style.cssText =
      'position:fixed;inset:0;z-index:2147483647;background:rgba(0,0,0,.55);' +
      'display:flex;align-items:center;justify-content:center;' +
      'font-family:system-ui,-apple-system,sans-serif;';
    const box = document.createElement('div');
    box.style.cssText =
      'background:#1e1f22;color:#fff;border-radius:12px;padding:24px 28px;' +
      'max-width:360px;width:calc(100% - 48px);text-align:center;' +
      'box-shadow:0 8px 32px rgba(0,0,0,.5);';
    const text = document.createElement('div');
    text.style.cssText = 'white-space:pre-wrap;font-size:14px;line-height:1.5;margin-bottom:20px;';
    text.textContent = message;
    const row = document.createElement('div');
    row.style.cssText = 'display:flex;gap:12px;justify-content:center;';
    const mkBtn = (label, css) => {
      const b = document.createElement('button');
      b.textContent = label;
      b.style.cssText =
        'padding:10px 20px;border:none;border-radius:8px;font-size:14px;' +
        'font-weight:600;cursor:pointer;' + css;
      return b;
    };
    let _settled = false;
    const finish = (ok) => {
      if (_settled) return;
      _settled = true;
      overlay.remove();
      resolve(ok);
    };
    const cancelBtn = mkBtn('Cancel', 'background:#4e5058;color:#fff;');
    const resetBtn = mkBtn('Reset anyway', 'background:#f23f43;color:#fff;');
    cancelBtn.addEventListener('click', () => finish(false));
    resetBtn.addEventListener('click', () => finish(true));
    row.appendChild(cancelBtn);
    row.appendChild(resetBtn);
    box.appendChild(text);
    box.appendChild(row);
    overlay.appendChild(box);
    document.body.appendChild(overlay);
    resetBtn.focus();
  });
}
function confirmCheatReset() {
  return _askInPage(_resetWarning);
}

if (isSmush) {
  // Smush (new Vite/React build): #smushResults auto-opens on game over; the share
  // card only renders in #smushShare (img.sharecard data: URL + .sharetxt text).
  // Note: the shared modal component portals #smushResults/#smushShare into <body>
  // at mount with inline display:none, so "element exists" does NOT mean "open" —
  // gate on computed visibility. Success locks permanently; a failed attempt (no
  // card) stays retryable via the observer.
  let _smushDone = false;
  let _smushInFlight = false;
  const _smushModalVisible = () =>
    [...document.querySelectorAll('#smushResults')]
      .some(el => getComputedStyle(el).display !== 'none');
  const _smushCapture = async () => {
    for (let i = 0; i < 60; i++) {
      const shareBtn = [...document.querySelectorAll('#smushResults button')]
        .find(b => (b.textContent || '').trim() === 'Share');
      if (shareBtn) shareBtn.click(); // opens #smushShare (no-op if already open)
      const img = document.querySelector('#smushShare img.sharecard[src^="data:image"]');
      if (img) {
        const textEl = document.querySelector('#smushShare .sharetxt');
        return { blob: await _cardBlobFromImg(img), text: textEl ? textEl.innerText : '' };
      }
      await _sleep(100);
    }
    return null;
  };
  const _trySmush = async () => {
    if (_smushDone || _smushInFlight) return;
    if (!_smushModalVisible()) return; // pre-rendered closed modal — wait for game over
    _smushInFlight = true;
    try {
      await _sleep(400); // let the results modal settle (community-stats fetch, animations)
      const c = await _smushCapture();
      if (c) { await _uploadScore(c.blob, c.text); _smushDone = true; }
      else console.warn('[' + gameTag + '] smush share card not found');
    } finally { _smushInFlight = false; }
  };
  const _startSmushObserver = () => {
    if (window.__fb3ObsSmush) return;
    _trySmush(); // covers a reload with the modal already open; no-op while closed
    window.__fb3ObsSmush = new MutationObserver(() => _trySmush());
    window.__fb3ObsSmush.observe(document.body, { childList: true, subtree: true });
  };
  _startSmushObserver();
  document.addEventListener('DOMContentLoaded', _startSmushObserver);

  // "Send to Discord" button in the results modal (manual send / re-send).
  const _addSmushButton = () => {
    const row = document.querySelector('#smushResults .rowbtns');
    if (!row || row.querySelector('#__fb3Send')) return;
    const btn = document.createElement('button');
    btn.id = '__fb3Send'; btn.type = 'button'; btn.className = 'hbtn';
    btn.textContent = 'Send to Discord';
    btn.style.cssText = 'background:#5865F2;color:#fff;border:none;cursor:pointer;font:inherit;padding:10px 16px;border-radius:8px;';
    btn.addEventListener('click', async (e) => {
      e.preventDefault();
      btn.disabled = true; btn.textContent = 'Sending…';
      try {
        const c = await _smushCapture();
        if (c) await _postScoreToDiscord(c.blob, c.text);
      } finally { btn.disabled = false; btn.textContent = 'Send to Discord'; }
    });
    row.appendChild(btn);
  };
  const _watchSmushButton = () => {
    if (window.__fb3BtnSmush) return;
    window.__fb3BtnSmush = new MutationObserver(() => _addSmushButton());
    window.__fb3BtnSmush.observe(document.body, { childList: true, subtree: true });
  };
  _watchSmushButton();
  document.addEventListener('DOMContentLoaded', _watchSmushButton);

  // Intercept the secret reset: 5 quick clicks on #title .logo. The game
  // counts its own clicks in a bubble listener on the logo element; we count
  // the same clicks at document level (capture phase) with the same 1.5s
  // rolling window, so our counter tracks the game's. On the click that would
  // trigger the reset we warn first: declining blocks the click before it
  // reaches the game (and re-arms on the next click), accepting sets the cheat
  // flag. The warning may take a while (in-page modal in Discord), so the
  // event is always blocked here and, on acceptance, we perform the game's
  // own reset — wipe save + reload — ourselves.
  let _resetTaps = 0, _resetTapTimer = null;
  document.addEventListener('click', (e) => {
    const t = e.target;
    if (!t || typeof t.closest !== 'function' || !t.closest('#title .logo')) return;
    if (_resetTaps >= 4) {
      e.preventDefault(); e.stopPropagation(); e.stopImmediatePropagation();
      confirmCheatReset().then((ok) => {
        if (!ok) { console.warn('[' + gameTag + '] board reset declined — save kept'); return; }
        markCheated();
        try { localStorage.removeItem(SAVEKEY); } catch (err) {}
        location.reload();
      });
      return;
    }
    _resetTaps++;
    clearTimeout(_resetTapTimer);
    _resetTapTimer = setTimeout(() => { _resetTaps = 0; }, 1500);
  }, true);
} else {
  // 4x3 (new Vite/React build): #postHub auto-renders on completion and holds the
  // share card at #postHub #phCard img (data: URL; alt = exact game share text).
  let _4x3Started = false;
  const _try4x3 = async () => {
    if (_4x3Started) return;
    if (!document.querySelector('#postHub')) return;
    _4x3Started = true; // single-flight: never re-run per mutation
    let img = null;
    for (let i = 0; i < 60 && !img; i++) {
      img = document.querySelector('#postHub #phCard img[src^="data:image"]');
      if (!img) await _sleep(100);
    }
    if (!img) { console.warn('[' + gameTag + '] share card not found in #postHub'); return; }
    try {
      const blob = await _cardBlobFromImg(img);
      await _uploadScore(blob, img.getAttribute('alt') || '');
    } catch(e) { console.warn('[' + gameTag + '] 4x3 capture failed', e); }
  };
  const _start4x3Observer = () => {
    if (window.__fb3Obs4x3) return;
    if (document.querySelector('#postHub')) { _try4x3(); return; }
    window.__fb3Obs4x3 = new MutationObserver(() => _try4x3());
    window.__fb3Obs4x3.observe(document.body, { childList: true, subtree: true });
  };
  _start4x3Observer();
  document.addEventListener('DOMContentLoaded', _start4x3Observer);

  // "Send to Discord" button on the result screen (manual re-send, no daily guard).
  const _add4x3Button = () => {
    const row = document.querySelector('#postHub .rowbtns');
    if (!row || row.querySelector('#__fb3Send')) return;
    const btn = document.createElement('button');
    btn.id = '__fb3Send'; btn.type = 'button'; btn.className = 'hbtn';
    btn.textContent = 'Send to Discord';
    btn.style.cssText = 'background:#5865F2;color:#fff;border:none;cursor:pointer;font:inherit;padding:10px 16px;border-radius:8px;';
    btn.addEventListener('click', async (e) => {
      e.preventDefault();
      const img = document.querySelector('#postHub #phCard img[src^="data:image"]');
      if (!img) return;
      btn.disabled = true; btn.textContent = 'Sending…';
      try { await _postScoreToDiscord(await _cardBlobFromImg(img), img.getAttribute('alt') || ''); }
      finally { btn.disabled = false; btn.textContent = 'Send to Discord'; }
    });
    row.appendChild(btn);
  };
  const _watch4x3Button = () => {
    if (window.__fb3Btn4x3) return;
    window.__fb3Btn4x3 = new MutationObserver(() => _add4x3Button());
    window.__fb3Btn4x3.observe(document.body, { childList: true, subtree: true });
  };
  _watch4x3Button();
  document.addEventListener('DOMContentLoaded', _watch4x3Button);

  // Intercept the secret 7-tap logo flow. The game's bubble listener on
  // header .logo counts 7 taps in a 1.5s window then calls askTestCode(), which
  // does prompt("") — blocked in Discord's sandboxed iframe (no allow-modals),
  // so the reset is unreachable there. We mirror the tap counting at the
  // document level (capture phase), block the 7th tap before it reaches the
  // game, and replace the native prompt with an in-page code modal. The game's
  // own "go home" guard for non-daily / beta / community boards is replicated
  // so we never interfere with the logo's navigation there.
  const _logoTapGoHome = () => {
    try {
      if (isBeta || communityId || (dailyKey && dailyKey !== todayStr())) return true;
    } catch (_) { /* globals not set yet — treat as a normal daily board */ }
    return false;
  };
  function _askCodeInPage(message) {
    // In-page replacement for the game's prompt("") — native prompts are
    // ignored (and logged) in Discord's sandboxed activity iframe.
    return new Promise((resolve) => {
      const overlay = document.createElement('div');
      overlay.id = '__fb3Code';
      overlay.style.cssText =
        'position:fixed;inset:0;z-index:2147483647;background:rgba(0,0,0,.55);' +
        'display:flex;align-items:center;justify-content:center;' +
        'font-family:system-ui,-apple-system,sans-serif;';
      const box = document.createElement('div');
      box.style.cssText =
        'background:#1e1f22;color:#fff;border-radius:12px;padding:24px 28px;' +
        'max-width:360px;width:calc(100% - 48px);text-align:center;' +
        'box-shadow:0 8px 32px rgba(0,0,0,.5);';
      const text = document.createElement('div');
      text.style.cssText = 'font-size:14px;line-height:1.5;margin-bottom:16px;';
      text.textContent = message;
      const input = document.createElement('input');
      input.type = 'text';
      input.autofocus = true;
      input.setAttribute('autocomplete', 'off');
      input.setAttribute('spellcheck', 'false');
      input.style.cssText =
        'width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid #4e5058;' +
        'border-radius:8px;background:#2b2d31;color:#fff;font-size:14px;outline:none;margin-bottom:16px;';
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;gap:12px;justify-content:center;';
      const mkBtn = (label, css) => {
        const b = document.createElement('button');
        b.textContent = label;
        b.style.cssText =
          'padding:10px 20px;border:none;border-radius:8px;font-size:14px;' +
          'font-weight:600;cursor:pointer;' + css;
        return b;
      };
      let _settled = false;
      const finish = (value) => {
        if (_settled) return;
        _settled = true;
        overlay.remove();
        resolve(value);
      };
      const ok = () => {
        const v = input.value.trim().toLowerCase();
        if (!v) { input.focus(); return; }
        finish(v);
      };
      const cancelBtn = mkBtn('Cancel', 'background:#4e5058;color:#fff;');
      const okBtn = mkBtn('OK', 'background:#f23f43;color:#fff;');
      cancelBtn.addEventListener('click', () => finish(null));
      okBtn.addEventListener('click', ok);
      input.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') { ev.preventDefault(); ok(); }
        else if (ev.key === 'Escape') { ev.preventDefault(); finish(null); }
      });
      row.appendChild(cancelBtn);
      row.appendChild(okBtn);
      box.appendChild(text);
      box.appendChild(input);
      box.appendChild(row);
      overlay.appendChild(box);
      document.body.appendChild(overlay);
      input.focus();
    });
  }
  async function _askTestCodeInPage() {
    // In-page replacement for the game's askTestCode(): same server-side check,
    // same openTestMenu() on success — but without the sandbox-blocked prompt().
    const w = (await _askCodeInPage('Enter the test code:')) || '';
    if (!w) return false;
    try {
      const r = await fetch('https://fourbythree-stats.hankmt.workers.dev/testcode', {
        method: 'POST',
        headers: { 'Content-Type': 'text/plain' },
        body: JSON.stringify({ w }),
      });
      const j = await r.json();
      if (j && j.ok) { window.openTestMenu(); return true; }
      console.warn('[' + gameTag + '] test code rejected');
    } catch (err) {
      console.warn('[' + gameTag + '] test code check failed', err);
    }
    return false;
  }
  let _logoTaps = 0, _logoTapTimer = null;
  document.addEventListener('click', (e) => {
    const t = e.target;
    if (!t || typeof t.closest !== 'function' || !t.closest('header .logo')) return;
    // Non-daily / beta / community boards: the logo click "goes home" instead
    // of counting taps — don't intercept that navigation.
    if (_logoTapGoHome()) return;
    if (_logoTaps >= 6) {
      e.preventDefault();
      e.stopPropagation();
      e.stopImmediatePropagation();
      _logoTaps = 0;
      confirmCheatReset().then((ok) => {
        if (!ok) { console.warn('[' + gameTag + '] test menu declined'); return; }
        _askTestCodeInPage();
      });
      return;
    }
    _logoTaps++;
    clearTimeout(_logoTapTimer);
    _logoTapTimer = setTimeout(() => { _logoTaps = 0; }, 1500);
  }, true);
}

