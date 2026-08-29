# Plan: CLI/Go-controlled proxy switching for Zen Browser

## Goal
Let a Go binary (your network-config switcher) change Zen's proxy live — no browser restart, no manual clicks — by talking to a small custom WebExtension over native messaging.

## Architecture

```
[Go network-config tool]
       │  stdin/stdout, native messaging protocol
       ▼
[Go native messaging host]  (separate small process, or same binary in "host mode")
       │  stdio, spawned by Zen
       ▼
[Zen extension: background.js]
       │  browser.proxy.settings.set()
       ▼
[Zen's live proxy config]
```

Two components you own: the extension (JS) and the native host (Go). Your existing switcher talks to the native host via a local IPC mechanism of your choice (see "Triggering" below) — it does not talk to Firefox's native messaging protocol directly, since that protocol is stdio-based and only Firefox can spawn the host.

## Component 1: The extension

**manifest.json** (Manifest V2 — Firefox still supports it and it's simpler for `proxy.settings`):
```json
{
  "manifest_version": 2,
  "name": "Proxy Switcher Bridge",
  "version": "1.0",
  "permissions": ["proxy", "nativeMessaging"],
  "background": { "scripts": ["background.js"] },
  "browser_specific_settings": {
    "gecko": { "id": "proxy-switcher-bridge@yourdomain" }
  }
}
```
- `"proxy"` permission → required for `browser.proxy.settings.set()`.
- `"nativeMessaging"` permission → required to talk to the native host.
- `browser_specific_settings.gecko.id` → needed so the native host manifest can allowlist this exact extension.

**background.js**:
```js
const port = browser.runtime.connectNative("com.yourname.proxyswitcher");

port.onMessage.addListener((msg) => {
  // msg example: { type: "socks5", host: "127.0.0.1", port: 1080 }
  // or: { type: "http", host: "10.0.0.5", port: 3128 }
  // or: { type: "direct" } / { type: "system" }
  const value = toProxyConfig(msg);
  browser.proxy.settings.set({ value }).then(
    () => port.postMessage({ ok: true }),
    (err) => port.postMessage({ ok: false, error: String(err) })
  );
});

function toProxyConfig(msg) {
  if (msg.type === "direct") return { proxyType: "none" };
  if (msg.type === "system") return { proxyType: "system" };
  if (msg.type === "socks5") return {
    proxyType: "manual",
    socks: msg.host, socksPort: msg.port, socksVersion: 5,
    proxyDNS: true,
  };
  if (msg.type === "http") return {
    proxyType: "manual",
    http: msg.host, httpPort: msg.port,
    ssl: msg.host, sslPort: msg.port,
  };
}
```

Note from the docs: if the extension doesn't have private-browsing access, `proxy.settings.set()` throws — grant it (or accept proxy only applies to non-private windows, check current behavior when you build this).

## Component 2: Native messaging host manifest

A JSON file (not the extension's manifest) that tells Zen/Firefox how to launch your Go binary. Name it e.g. `com.yourname.proxyswitcher.json`:

```json
{
  "name": "com.yourname.proxyswitcher",
  "description": "Bridge between network switcher and Zen proxy settings",
  "path": "/usr/local/bin/proxyswitcher-host",
  "type": "stdio",
  "allowed_extensions": ["proxy-switcher-bridge@yourdomain"]
}
```

Install location (must match the `name` field, filename = `<name>.json`):
- **macOS**: `~/Library/Application Support/Mozilla/NativeMessagingHosts/`
- **Linux**: `/usr/lib/mozilla/native-messaging-hosts/` (or `/usr/lib64/...`, or `~/.mozilla/native-messaging-hosts/` for per-user)
- **Windows**: file anywhere + a registry entry at `HKEY_CURRENT_USER\Software\Mozilla\NativeMessagingHosts\<name>` whose default value is the manifest's path

Your Go build/install step should drop this file (and the binary) in place. Zen is Firefox-based so it reads the same locations as Firefox — worth confirming Zen doesn't sandbox/rename its native messaging dirs when you get to implementation, but as of now Zen inherits Gecko's native messaging stack unmodified.

## Component 3: The Go native messaging host

Firefox's native messaging wire format: each message is UTF-8 JSON, prefixed by a 4-byte little-endian length, over stdin/stdout. Max message size ~1MB (host→app) / 4GB (app→host as of Firefox 66, but Zen is desktop-only so no mobile-size limit worries).

Go side needs:
1. A read loop: read 4 bytes → length N → read N bytes → unmarshal JSON.
2. A write function: marshal JSON → write 4-byte length header → write bytes.
3. Firefox spawns this process on `connectNative()` and kills it when the port disconnects — so this binary should be a short-lived process per connection, not a long-running daemon by itself.

Existing Go libraries exist for this framing (search "golang native messaging" or "chrome native messaging go") — worth using one instead of hand-rolling, since the framing is easy to get subtly wrong (endianness, partial reads).

## Triggering: how your switcher app reaches the native host

The native host process only exists while Zen has it spawned (i.e., while the extension's background script is alive and has called `connectNative`). Your switcher app can't just launch the Go binary standalone and expect it to talk to Zen — it needs a live connection Zen already opened. Options, in order of robustness:

1. **Long-lived host, local socket bridge (recommended).** Have the native host, once spawned by Zen, open a local Unix socket (or named pipe on Windows) at a well-known path, e.g. `/tmp/zen-proxy-switcher.sock`. Your network-config Go binary connects to that socket and sends proxy-change requests; the native host forwards them over stdio to the extension. If Zen isn't running (no socket present), your switcher just skips the browser update and moves on — simple, decoupled.
2. **Extension polls a file.** Simpler but hackier: background.js polls (e.g. every 1s via `setInterval`) a config file your switcher writes to, and calls `proxy.settings.set()` when it changes. No native messaging needed at all, but polling delay and no confirmation channel back to the switcher.
3. **Extension listens on a local HTTP port** (via a content script is not needed — background scripts can use `fetch`/`WebSocket` directly). Background.js runs a small WebSocket client connecting to a local server your Go switcher hosts; switcher pushes proxy changes over the socket. This avoids native messaging and its manifest-file installation dance entirely — arguably the simplest reliable option if you're fine with the extension needing `host_permissions` for `ws://127.0.0.1/*` and your switcher exposing a local listener.

Given the installation friction of native messaging manifests (per-OS paths, registry on Windows), **option 3 (background script as WebSocket client, Go switcher as WebSocket server) is likely the least fragile** for a personal tool — no manifest file to install/uninstall, no OS-specific paths, works identically cross-platform. Native messaging (option 1) is more "correct" for store-distributed extensions but adds packaging overhead not worth it for a personal switcher.

## Security notes for later
- Bind any local socket/WebSocket server to `127.0.0.1` only, never `0.0.0.0`.
- If using a socket file, set restrictive permissions (`0600`) so other local users can't push proxy changes.
- Native messaging manifest `allowed_extensions` must list the exact extension ID — don't wildcard it.

## Suggested build order
1. Minimal extension with hardcoded `proxy.settings.set()` call, verify it actually changes Zen's proxy (check `about:preferences#general` proxy section or `about:config` → `network.proxy.*`).
2. Add WebSocket client in background.js, stand up a trivial Go WebSocket echo server, confirm round-trip.
3. Wire real proxy-change messages from your network-config tool into that WebSocket server.
4. Add reconnect/retry logic in background.js (Zen restarts, tool restarts — both need to resync).
5. If you later want to distribute this beyond your own machine, revisit native messaging (option 1) for a more "installable" story.
