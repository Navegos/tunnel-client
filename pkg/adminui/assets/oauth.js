(function () {
  const adminUI = (window.adminUI = window.adminUI || {});
  const $ = adminUI.$ || ((id) => document.getElementById(id));
  const fetchJSON =
    adminUI.fetchJSON ||
    (async (path) => {
      const res = await fetch(path, { cache: "no-store" });
      if (!res.ok) throw new Error(res.status + " " + res.statusText);
      return await res.json();
    });
  const fmtUptime =
    adminUI.fmtUptime ||
    ((seconds) => {
      seconds = Math.max(0, Math.floor(seconds || 0));
      const d = Math.floor(seconds / 86400);
      seconds -= d * 86400;
      const h = Math.floor(seconds / 3600);
      seconds -= h * 3600;
      const m = Math.floor(seconds / 60);
      seconds -= m * 60;
      const parts = [];
      if (d) parts.push(d + "d");
      if (h) parts.push(h + "h");
      if (m) parts.push(m + "m");
      parts.push(seconds + "s");
      return parts.join(" ");
    });

  const discoveryOrder = [
    { priority: 1, source: "www_authenticate" },
    { priority: 2, source: "well_known_path" },
    { priority: 3, source: "well_known_root" },
  ];

  function statusForAttempt(attempt, pending) {
    if (attempt && attempt.selected) return "selected";
    if (attempt && attempt.tried) return "tried";
    if (attempt) return "skipped";
    if (pending) return "pending";
    return "skipped";
  }

  function fallbackURLs(discoveryUrls) {
    const urls = Array.isArray(discoveryUrls) ? discoveryUrls : [];
    const out = { well_known_path: "", well_known_root: "" };
    if (urls.length === 1) {
      out.well_known_root = urls[0];
    } else if (urls.length >= 2) {
      if (urls.length >= 3) {
        out.well_known_path = urls[1];
        out.well_known_root = urls[2];
      } else {
        out.well_known_path = urls[0];
        out.well_known_root = urls[1];
      }
    }
    return out;
  }

  function renderDiscoveryTable(payload) {
    const tbody = $("oauthDiscoveryRows");
    if (!tbody) return;
    tbody.textContent = "";

    const pending = payload ? payload.pending : false;
    const probe = payload ? payload.www_authenticate_probe : null;
    const attempts =
      payload && payload.metadata && Array.isArray(payload.metadata.attempts)
        ? payload.metadata.attempts
        : [];
    const fallback = fallbackURLs(payload ? payload.discovery_urls : []);
    const attemptsBySource = new Map();
    attempts.forEach((attempt) => {
      if (attempt && attempt.source) {
        attemptsBySource.set(attempt.source, attempt);
      }
    });

    discoveryOrder.forEach(({ priority, source }) => {
      const attempt = attemptsBySource.get(source);
      let status = statusForAttempt(attempt, pending);
      let urlText = attempt ? attempt.url : "";
      let selected = attempt ? !!attempt.selected : false;
      let placeholder = false;

      if (source === "www_authenticate") {
        if (probe && probe.attempted && !probe.url) {
          status = "skipped";
          urlText = "WWW-Authenticate resource_metadata not implemented";
          selected = false;
          placeholder = true;
        } else if (!urlText && probe && probe.url) {
          urlText = probe.url;
        }
        if (!urlText && !placeholder) {
          urlText = "—";
          placeholder = true;
        }
      } else {
        if (!urlText) {
          urlText =
            source === "well_known_path"
              ? fallback.well_known_path
              : fallback.well_known_root;
        }
        if (!urlText) {
          urlText = "—";
          placeholder = true;
        }
      }

      const row = document.createElement("tr");

      const priorityCell = document.createElement("td");
      priorityCell.textContent = String(priority);

      const typeCell = document.createElement("td");
      typeCell.textContent = source;

      const statusCell = document.createElement("td");
      const statusDiv = document.createElement("div");
      statusDiv.className = "oauth-status";
      statusDiv.textContent = status;
      const urlDiv = document.createElement("div");
      urlDiv.className = "oauth-url";
      if (selected) urlDiv.classList.add("selected");
      if (placeholder) urlDiv.classList.add("placeholder");
      urlDiv.textContent = urlText;
      statusCell.appendChild(statusDiv);
      statusCell.appendChild(urlDiv);

      row.appendChild(priorityCell);
      row.appendChild(typeCell);
      row.appendChild(statusCell);
      tbody.appendChild(row);
    });
  }

  function fmtTimestamp(ts) {
    if (!ts) return "—";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return ts;
    const ageSec = Math.floor((Date.now() - d.getTime()) / 1000);
    return d.toISOString() + " (" + fmtUptime(ageSec) + " ago)";
  }

  function renderOAuthMetadata(meta, err, pending, metadataSource) {
    const statusEl = $("oauthStatus");
    if (!statusEl) return;
    if (err) {
      statusEl.textContent = "error";
    } else if (pending) {
      statusEl.textContent = "pending";
    } else if (meta) {
      statusEl.textContent = meta.status_code || "—";
    } else {
      statusEl.textContent = "—";
    }
    const fetchedEl = $("oauthFetchedAt");
    if (fetchedEl) {
      fetchedEl.textContent = meta ? fmtTimestamp(meta.fetched_at) : "—";
    }
    const sourceEl = $("oauthSourceUrl");
    if (sourceEl) {
      sourceEl.textContent = meta ? meta.url || "—" : "—";
    }
    const metaSourceEl = $("oauthMetadataSource");
    if (metaSourceEl) {
      metaSourceEl.textContent = metadataSource || "—";
    }

    const headersEl = $("oauthHeaders");
    const bodyEl = $("oauthBody");
    if (!headersEl || !bodyEl) return;

    if (err) {
      headersEl.textContent = "";
      bodyEl.textContent = "";
      return;
    }

    const headers = meta && meta.headers ? meta.headers : null;
    headersEl.textContent = headers ? JSON.stringify(headers, null, 2) : "";

    if (meta) {
      if (meta.body) {
        bodyEl.textContent = JSON.stringify(meta.body, null, 2);
      } else if (meta.body_text) {
        bodyEl.textContent = meta.body_text;
      } else {
        bodyEl.textContent = "";
      }
    } else {
      bodyEl.textContent = "";
    }
  }

  async function refreshOAuth() {
    const errEl = $("oauthErr");
    if (errEl) errEl.textContent = "";
    try {
      const o = await fetchJSON("/api/oauth");
      renderDiscoveryTable(o);
      renderOAuthMetadata(o.metadata, o.error, o.pending, o.metadata_source);
      if (o.error && errEl) {
        errEl.textContent = o.error;
      }
    } catch (e) {
      if (errEl) errEl.textContent = "error: " + e;
      renderDiscoveryTable(null);
      renderOAuthMetadata(null, e, false, "");
    }
  }

  const refreshButton = $("refreshOAuth");
  if (refreshButton) {
    refreshButton.addEventListener("click", refreshOAuth);
  }

  adminUI.oauth = {
    refresh: refreshOAuth,
  };
})();
