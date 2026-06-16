# FourByThree Discord App

A Discord Activity that lets you play Hank Green's daily [4×3 puzzle](https://www.hankgreen.com/fourbythree) directly inside a Discord voice channel.

The game itself is served directly from hankgreen.com — this project is just the Discord Activity shell that wraps it.

## How it works

```
Discord Activity iframe (discordsays.com)
    → GET /                            (our Go server — wrapper page)
    → iframe src="/hankgreen/fourbythree"
         → Locally:   Go reverse proxy → www.hankgreen.com
         → In Discord: Dev Portal URL Mapping → www.hankgreen.com
```

The Go server is deliberately minimal:
1. Serves a wrapper HTML page that initialises the Discord Embedded App SDK
2. In local dev, reverse-proxies `/hankgreen/*` to `www.hankgreen.com`
3. No OAuth2 or user data — the game doesn't need Discord identity

## Setup

### 1. Discord Developer Portal

1. Go to [discord.com/developers/applications](https://discord.com/developers/applications) → create a new Application
2. Under **Activities**, enable the Activity feature
3. Under **URL Mappings**, add:
   | Prefix | Target |
   |---|---|
   | `/` | `your-server-hostname.com` (your tunnel/VPS URL — no `https://`) |
   | `/hankgreen` | `www.hankgreen.com` |
4. Copy your **Application (Client) ID**

### 2. Configure environment

```bash
cp .env.example .env
# Edit .env and set DISCORD_CLIENT_ID to your Application ID
```

### 3. Fetch the Discord SDK

`client/static/` is gitignored — it holds the vendored [Discord Embedded App SDK](https://www.npmjs.com/package/@discord/embedded-app-sdk) and must be populated before building or running:

```bash
npm pack @discord/embedded-app-sdk@1
mkdir -p client/static
tar -xzf discord-embedded-app-sdk-*.tgz --strip-components=2 -C client/static/ package/output
rm discord-embedded-app-sdk-*.tgz
```

This extracts the ESM build into `client/static/` where Go embeds it at compile time. To upgrade the SDK, re-run the same commands with the new version.

### 4. Run locally

```bash
# Load env vars and run
source .env
go run main.go
```

Open [http://localhost:3000](http://localhost:3000) — the game loads directly in your browser.

### 5. Launch in Discord

In any voice channel → **Start Activity** → select **4×3**

## Production (NGINX)

hankgreen.com uses Next.js streaming (chunked transfer encoding). Without the right NGINX directives, the proxy buffers the response and the client waits 60 s+ for the stream to close. Use this config:

```nginx
location / {
    proxy_pass http://localhost:4343;
    proxy_http_version 1.1;       # required for chunked encoding
    proxy_set_header Connection ""; # enable keep-alive to upstream
    proxy_set_header Host $host;
    proxy_buffering off;           # stream chunks immediately to the client
}
```


## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `DISCORD_CLIENT_ID` | `YOUR_CLIENT_ID` | Discord Application Client ID |
| `PORT` | `4343` | HTTP listen port |
