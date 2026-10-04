package browser

// authCaptureScript records only Bearer tokens that the Fomo application itself
// sends to prod-api.fomo.family. Earlier versions recorded every Authorization
// header on the page, which could capture Privy PAT / unrelated Bearer tokens
// and then accidentally use them as the Fomo API access token.
const authCaptureScript = `(() => {
  if (window.__FOMO_AUTH_CAPTURE_INSTALLED__) return;
  window.__FOMO_AUTH_CAPTURE_INSTALLED__ = true;
  window.__FOMO_LAST_API_BEARER__ = window.__FOMO_LAST_API_BEARER__ || "";
  window.__FOMO_LAST_API_BEARER_AT__ = window.__FOMO_LAST_API_BEARER_AT__ || 0;
  window.__FOMO_LAST_API_BEARER_URL__ = window.__FOMO_LAST_API_BEARER_URL__ || "";

  const isFomoAPI = (raw) => {
    try {
      const u = new URL(String(raw || ""), location.href);
      return u.hostname === "prod-api.fomo.family";
    } catch (_) {
      return false;
    }
  };

  const remember = (url, name, value) => {
    try {
      if (!isFomoAPI(url)) return;
      if (!name || String(name).toLowerCase() !== "authorization") return;
      const v = String(value || "");
      if (!v.toLowerCase().startsWith("bearer ")) return;
      window.__FOMO_LAST_API_BEARER__ = v.slice(7).trim();
      window.__FOMO_LAST_API_BEARER_AT__ = Date.now();
      window.__FOMO_LAST_API_BEARER_URL__ = String(url || "");
    } catch (_) {}
  };

  const inspectHeaders = (url, headers) => {
    try {
      if (!headers) return;
      if (headers instanceof Headers) {
        const v = headers.get("authorization");
        if (v) remember(url, "authorization", v);
        return;
      }
      if (Array.isArray(headers)) {
        for (const [k, v] of headers) remember(url, k, v);
        return;
      }
      if (typeof headers === "object") {
        for (const [k, v] of Object.entries(headers)) remember(url, k, v);
      }
    } catch (_) {}
  };

  const originalFetch = window.fetch;
  window.fetch = function(input, init) {
    try {
      const url = input instanceof Request ? input.url : String(input || "");
      if (input instanceof Request) inspectHeaders(url, input.headers);
      if (init) inspectHeaders(url, init.headers);
    } catch (_) {}
    return originalFetch.apply(this, arguments);
  };

  const originalOpen = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(method, url) {
    try { this.__FOMO_CAPTURE_URL__ = String(url || ""); } catch (_) {}
    return originalOpen.apply(this, arguments);
  };

  const originalSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.setRequestHeader = function(name, value) {
    try { remember(this.__FOMO_CAPTURE_URL__ || "", name, value); } catch (_) {}
    return originalSetRequestHeader.apply(this, arguments);
  };
})();`

// findTokenScript deliberately selects Privy's ACCESS token (privy:token), not
// privy:pat. PAT and refresh_token belong to Privy's session-renewal machinery;
// they are not the Bearer accepted by Fomo's prod-api.
//
// Priority:
//  1. exact localStorage "privy:token"
//  2. namespaced Privy keys ending in ":token"
//  3. a Bearer observed on a request specifically to prod-api.fomo.family
const findTokenScript = `(() => {
  const candidates = [];

  const decode = (raw, source, priority) => {
    if (typeof raw !== "string") return;
    let s = raw.trim();
    // Privy values can occasionally be JSON-string encoded in localStorage.
    try {
      const parsed = JSON.parse(s);
      if (typeof parsed === "string") s = parsed;
    } catch (_) {}

    const matches = s.match(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g);
    if (!matches) return;
    for (const token of matches) {
      try {
        const p = token.split('.')[1].replace(/-/g,'+').replace(/_/g,'/');
        const padded = p + '='.repeat((4 - p.length % 4) % 4);
        const payload = JSON.parse(atob(padded));
        const exp = Number(payload.exp || 0);
        if (exp > Date.now()/1000) candidates.push({token, exp, source, priority});
      } catch (_) {}
    }
  };

  try {
    const exact = localStorage.getItem("privy:token");
    if (exact) decode(exact, "localStorage:privy:token", 300);

    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i) || "";
      if (k === "privy:token") continue;
      // Support Privy's namespaced access-token keys while explicitly excluding
      // refresh_token and pat.
      if (/^privy:.*:token$/i.test(k) && !/refresh_token/i.test(k) && !/:pat$/i.test(k)) {
        decode(localStorage.getItem(k), "localStorage:" + k, 200);
      }
    }

    if (window.__FOMO_LAST_API_BEARER__) {
      decode(window.__FOMO_LAST_API_BEARER__, "captured-fomo-api-request", 400);
    }
  } catch (_) {}

  candidates.sort((a,b) => (b.priority - a.priority) || (b.exp - a.exp));
  return candidates[0] || null;
})()`
