// Launcher script — runs in the Activity iframe root.
// Template placeholder: {{.ClientID}} is replaced at startup.

const inDiscord = new URLSearchParams(window.location.search).has('frame_id');
let authenticatedUserId = null;

if (inDiscord) {
  const { DiscordSDK } = await import('/static/index.mjs');
  const sdk = new DiscordSDK({{.ClientID}});
  window.discordSdk = sdk;
  await sdk.ready();
  console.log('[launcher] Discord SDK ready');

  try {
    const { code } = await sdk.commands.authorize({
      client_id: {{.ClientID}},
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
    console.log('[launcher] Authenticated user', authenticatedUserId);

    const statusRes = await fetch('api/status?user_id=' + authenticatedUserId);
    if (!statusRes.ok) {
      if (typeof window.toast === 'function') {
        window.toast("Start the game with /play4x3 or /playsmush to auto-share your score!", 5000);
      }
    } else {
      const statusData = await statusRes.json();
      // Inject child iframe for the game — avoids Discord mobile block on window.location.replace().
      const gamePath = statusData.game_type === 'smush' ? '/smush/' : '/fourbythree/';
      const iframe = document.createElement('iframe');
      iframe.id = '_gameIframe';
      iframe.src = gamePath + '?_uid=' + authenticatedUserId + '&_cid=' + window.discordSdk.channelId + window.location.search + window.location.hash;
      iframe.style.cssText = 'position:fixed;top:0;left:0;width:100vw;height:100vh;border:none;';
      document.body.appendChild(iframe);
      console.log('[launcher] Loaded game iframe:', iframe.src);
    }
  } catch(e) {
    console.error('[launcher] Auth failed', e);
  }
} else {
  console.log('[launcher] Running outside Discord');
}
