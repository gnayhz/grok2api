// 代理/订阅 URL 的前端预校验:与后端 normalize/validate 同口径。
// 后端保持权威;这里只做快速反馈。订阅拉取代理不允许使用按账号占位符。

/** Performs fast client-side proxy validation; the backend remains authoritative. */
function validProxyURL(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed.length === 0) return true;
  if (trimmed.length > 8192 || [...trimmed].some((char) => {
    const code = char.charCodeAt(0);
    return code <= 0x1f || code === 0x7f;
  })) return false;
  if ((trimmed.match(/\{account\}/g) ?? []).length > 1) return false;
  const scheme = trimmed.slice(0, trimmed.indexOf(":")).toLowerCase();
  if (["trojan", "vless", "ss", "vmess"].includes(scheme) && trimmed.includes("{account}")) return false;
  if (scheme === "vmess") return validVMessURL(trimmed);
  if (scheme === "ss") return validShadowsocksURL(trimmed);
  try {
    const parseValue = trimmed.replaceAll("{account}", "grok2api_account_placeholder");
    const parsed = new URL(parseValue);
    if (!parsed.host || !parsed.hostname) return false;
    const parsedScheme = parsed.protocol.replace(/:$/, "").toLowerCase();
    if (["trojan", "vless"].includes(parsedScheme)) {
      if (!parsed.username || parsed.password || !parsed.port) return false;
      const transport = (parsed.searchParams.get("type") ?? parsed.searchParams.get("network") ?? "tcp").toLowerCase();
      if (!["tcp", "none", "ws", "websocket"].includes(transport)) return false;
      const security = (parsed.searchParams.get("security") ?? "").toLowerCase();
      if (!["", "none", "tls"].includes(security)) return false;
      if (parsed.searchParams.get("flow")) return false;
      if (parsedScheme === "vless" && !/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(parsed.username)) return false;
      if (parsedScheme === "vless" && !["", "none"].includes((parsed.searchParams.get("encryption") ?? "").toLowerCase())) return false;
      if (!["", "none"].includes((parsed.searchParams.get("headerType") ?? "").toLowerCase())) return false;
      if (!validTunnelBoolean(parsed.searchParams, ["allowInsecure", "insecure", "skip-cert-verify"])) return false;
      if (parsed.pathname !== "" && parsed.pathname !== "/") return false;
      const webSocket = transport === "ws" || transport === "websocket";
      if (!webSocket && (parsed.searchParams.has("host") || parsed.searchParams.has("path"))) return false;
      if (webSocket) {
        const host = parsed.searchParams.get("host") ?? parsed.searchParams.get("sni") ?? parsed.searchParams.get("peer") ?? parsed.hostname;
        if (!validWebSocketHost(host)) return false;
      }
      return true;
    }
    if (!["http", "https", "socks4", "socks4a", "socks5", "socks5h"].includes(parsedScheme)) return false;
    if (parsed.search || parsed.hash || (parsed.pathname !== "" && parsed.pathname !== "/")) return false;
    if (trimmed.includes("{account}")) {
      if (!parsed.username.includes("grok2api_account_placeholder")) return false;
    }
    return true;
  } catch {
    return false;
  }
}

function validShadowsocksURL(value: string): boolean {
  try {
    let payload = value.slice("ss://".length).split("#", 1)[0];
    const queryIndex = payload.indexOf("?");
    if (queryIndex >= 0) {
      if ([...new URLSearchParams(payload.slice(queryIndex + 1)).keys()].length !== 0) return false;
      payload = payload.slice(0, queryIndex);
    }
    let credentials: string;
    let server: string;
    const separator = payload.lastIndexOf("@");
    if (separator >= 0) {
      credentials = decodeURIComponent(payload.slice(0, separator));
      if (!credentials.includes(":")) credentials = decodeBase64Text(credentials);
      server = payload.slice(separator + 1);
    } else {
      const decoded = decodeBase64Text(payload);
      const legacySeparator = decoded.lastIndexOf("@");
      if (legacySeparator < 0) return false;
      credentials = decoded.slice(0, legacySeparator);
      server = decoded.slice(legacySeparator + 1);
    }
    const credentialSeparator = credentials.indexOf(":");
    if (credentialSeparator <= 0 || credentialSeparator === credentials.length - 1) return false;
    const method = credentials.slice(0, credentialSeparator).trim().toLowerCase();
    if (!["aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"].includes(method)) return false;
    const parsed = new URL(`ss://${server}`);
    return parsed.username === "" && parsed.password === "" && parsed.hostname !== "" && parsed.port !== ""
      && (parsed.pathname === "" || parsed.pathname === "/") && parsed.search === "" && parsed.hash === "";
  } catch {
    return false;
  }
}

function decodeBase64Text(value: string): string {
  let payload = value.replaceAll("-", "+").replaceAll("_", "/");
  payload += "=".repeat((4 - (payload.length % 4)) % 4);
  const bytes = Uint8Array.from(atob(payload), (character) => character.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

function validTunnelBoolean(query: URLSearchParams, names: string[]): boolean {
  for (const name of names) {
    if (!query.has(name)) continue;
    return ["", "0", "1", "false", "true", "no", "yes"].includes((query.get(name) ?? "").trim().toLowerCase());
  }
  return true;
}

function validWebSocketHost(value: string): boolean {
  try {
    const parsed = new URL(`http://${value.trim()}`);
    return parsed.hostname !== "" && parsed.username === "" && parsed.password === ""
      && (parsed.pathname === "" || parsed.pathname === "/") && parsed.search === "" && parsed.hash === "";
  } catch {
    return false;
  }
}

function validVMessURL(value: string): boolean {
  try {
    let payload = value.slice("vmess://".length).split("#", 1)[0].replaceAll("-", "+").replaceAll("_", "/");
    payload += "=".repeat((4 - (payload.length % 4)) % 4);
    const bytes = Uint8Array.from(atob(payload), (character) => character.charCodeAt(0));
    const config = JSON.parse(new TextDecoder().decode(bytes)) as Record<string, unknown>;
    const id = String(config.id ?? "");
    const port = Number(config.port);
    const network = String(config.net ?? "tcp").toLowerCase();
    const tls = String(config.tls ?? "").toLowerCase();
    const cipher = String(config.scy ?? config.security ?? "auto").toLowerCase();
    const headerType = String(config.type ?? "").toLowerCase();
    const alterID = Number(config.aid ?? 0);
    const webSocket = network === "ws" || network === "websocket";
    const host = String(config.host ?? config.sni ?? config.add ?? "");
    return typeof config.add === "string" && config.add.trim() !== "" && Number.isInteger(port) && port > 0 && port <= 65535
      && /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(id)
      && Number.isInteger(alterID) && alterID >= 0 && alterID <= 65535
      && ["tcp", "ws", "websocket"].includes(network) && ["", "none", "tls"].includes(tls)
      && ["auto", "aes-128-gcm", "chacha20-poly1305", "none"].includes(cipher)
      && ["", "none"].includes(headerType)
      && validJSONTunnelBoolean(config.allowInsecure)
      && (webSocket ? validWebSocketHost(host) : String(config.path ?? "") === "" && String(config.host ?? "") === "");
  } catch {
    return false;
  }
}

function validJSONTunnelBoolean(value: unknown): boolean {
  if (value == null || typeof value === "boolean") return true;
  if (typeof value === "number") return value === 0 || value === 1;
  if (typeof value === "string") return ["", "0", "1", "false", "true", "no", "yes"].includes(value.trim().toLowerCase());
  return false;
}

/** Subscription fetch proxies must never use per-account lease placeholders. */
export function validSubscriptionProxyURL(value: string): boolean {
  return !value.includes("{account}") && validProxyURL(value);
}

// 订阅地址与后端 normalizeSubscriptionURL 同口径:HTTP(S)、有主机名、无
// 片段、无控制字符、长度 <= 8192。
export function validSubscriptionURL(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > 8192) return false;
  // eslint-disable-next-line no-control-regex -- 与后端控制字符校验同口径 (0x00-0x1f, 0x7f)
  if (/[\u0000-\u001f\u007f]/.test(trimmed)) return false;
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return false;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return false;
  if (!parsed.hostname) return false;
  return !parsed.hash;
}
