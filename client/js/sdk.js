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

// === Core share function — posts score image to Discord via the Go proxy. ===
// Used by _uploadScore (with daily guard), the 4x3 showResult hook, and the footer button.
const _postScoreToDiscord = async () => {
  if (!authenticatedUserId) {
    console.warn('[' + gameTag + '] Not authenticated');
    return;
  }
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
        formData.append('text_result',
          typeof window.shareText === 'function' ? window.shareText() :
          (typeof window.resultDescription === 'function' ? window.resultDescription() : ''));
      } catch(e) {}
      if (window.__puzzleLink) { formData.append('puzzle_link', window.__puzzleLink); }
      const _cheatedDate = getCheatedDate();
      if (_cheatedDate) { formData.append('cheated', _cheatedDate); }
      const res = await fetch('api/result', { method: 'POST', body: formData });
      if (res.ok) {
        console.log('[' + gameTag + '] Score shared to chat!');
        if (typeof window.toast === 'function') window.toast("Score shared to chat!", 3000);
      } else {
        console.warn('[' + gameTag + '] Share failed');
      }
    });
  } catch(e) { console.error('[' + gameTag + '] Upload failed', e); }
};

// === Auto-upload with daily guard (called by game completion observers) ===
const _uploadScore = async () => {
  const todayKey = gameTag + '_uploaded_' + new Date().toDateString();
  if (window['__' + gameTag + 'Uploaded'] || !inDiscord || !authenticatedUserId || localStorage.getItem(todayKey)) return;
  window['__' + gameTag + 'Uploaded'] = true;
  localStorage.setItem(todayKey, '1');
  await _postScoreToDiscord();
};

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
  // 4x3: hook showResult — only upload once per game.
  // Set __uploadedResult = true BEFORE the async toBlob call to block
  // rapid successive showResult() calls that can race the flag.
  const _hookShowResult = () => {
    if (typeof window.showResult !== 'function') { setTimeout(_hookShowResult, 200); return; }
    const _origShowResult = window.showResult;
    window.showResult = function(won, score, fancy, quiet) {
      _origShowResult.apply(this, arguments);
      const todayKey = '4x3_uploaded_' + new Date().toDateString();
      if (window.__uploadedResult || localStorage.getItem(todayKey) || !inDiscord || !authenticatedUserId) return;
      window.__uploadedResult = true;
      localStorage.setItem(todayKey, '1');
      _postScoreToDiscord();
    };
  };
  _hookShowResult();

  // Intercept the secret reset: the up/up/down/down/up/up key combo and the
  // hidden test menu's "Reset puzzle" button both call the global
  // resetPuzzle(), so wrapping that single choke point covers both.
  const _hookResetPuzzle = () => {
    if (typeof window.resetPuzzle !== 'function') { setTimeout(_hookResetPuzzle, 200); return; }
    if (window.__resetHooked) return;
    window.__resetHooked = true;
    const _origReset = window.resetPuzzle;
    window.resetPuzzle = function(...args) {
      // If today's cheat warning was already accepted (e.g. the user opened the
      // test menu via the logo-tap flow and then clicked "Reset puzzle"), do not
      // prompt a second time — just perform the reset.
      if (getCheatedDate()) { _origReset.apply(this, args); return; }
      confirmCheatReset().then((ok) => {
        if (!ok) { console.warn('[' + gameTag + '] puzzle reset declined — progress kept'); return; }
        markCheated();
        _origReset.apply(this, args);
      });
    };
  };
  _hookResetPuzzle();

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

// === Footer "Share to Discord" button (no daily guard — for re-entry after timeout) ===
const _addShareButton = () => {
  if (window.__shareButtonAdded) return;
  const footLinks = document.querySelector('.foot-links');
  if (!footLinks) return;
  window.__shareButtonAdded = true;
  const btn = document.createElement('a');
  btn.textContent = 'Share to Discord';
  btn.style.cursor = 'pointer';
  btn.addEventListener('click', (e) => {
    e.preventDefault();
    _postScoreToDiscord();
  });
  footLinks.appendChild(btn);
};
_addShareButton();
document.addEventListener('DOMContentLoaded', _addShareButton);
