(function () {
  const adminUI = (window.adminUI = window.adminUI || {});
  const $ = adminUI.$ || ((id) => document.getElementById(id));

  function ensurePanel() {
    const mount = $("harpoon-mount");
    const template = document.getElementById("harpoon-template");
    if (!mount || !template || $("panel-harpoon")) return;
    const clone = document.importNode(template.content, true);
    mount.appendChild(clone);
  }

  ensurePanel();

  const statusBadge = $("harpoonStatusBadge");
  const captureBadge = $("harpoonCaptureBadge");
  const disabledEl = $("harpoonDisabled");
  const contentEl = $("harpoonContent");
  const targetsEl = $("harpoonTargets");
  const callsEl = $("harpoonCalls");
  const labelFilterEl = $("harpoonLabelFilter");
  const errEl = $("harpoonErr");
  const refreshBtn = $("harpoonRefresh");
  const policyAllowPlaintextEl = $("harpoonPolicyAllowPlaintext");
  const policyMaxResponseEl = $("harpoonPolicyMaxResponseBytes");
  const policyMaxRedirectsEl = $("harpoonPolicyMaxRedirects");

  if (!statusBadge || !captureBadge || !labelFilterEl || !callsEl) return;

  let active = false;
  let poller = null;
  let captureEnabled = false;
  let availableLabels = [];
  const openCalls = new Set();
  const responseView = new Map();

  function setBadge(el, kind, text) {
    el.className = "badge " + kind;
    el.textContent = text;
  }

  function setDisabledState(disabled, reason) {
    if (disabledEl) {
      disabledEl.style.display = disabled ? "block" : "none";
      disabledEl.textContent = reason || "Harpoon disabled";
    }
    if (contentEl) {
      contentEl.style.display = disabled ? "none" : "grid";
    }
  }

  function formatBytes(bytes) {
    if (bytes == null) return "—";
    return bytes.toString();
  }

  function tryPrettyJSON(text) {
    if (!text) return "";
    try {
      const parsed = JSON.parse(text);
      return JSON.stringify(parsed, null, 2);
    } catch (e) {
      return text;
    }
  }

  function formatPayload(body, isBase64) {
    if (!body) return "—";
    if (isBase64) {
      return "Base64 payload:\n" + body;
    }
    return tryPrettyJSON(body);
  }

  function renderTargets(targets) {
    if (!targetsEl) return;
    targetsEl.textContent = "";
    if (!targets || targets.length === 0) {
      const row = document.createElement("tr");
      const cell = document.createElement("td");
      cell.colSpan = 3;
      cell.className = "muted";
      cell.textContent = "No targets configured.";
      row.appendChild(cell);
      targetsEl.appendChild(row);
      return;
    }

    targets.forEach((t) => {
      const row = document.createElement("tr");
      row.innerHTML =
        "<td class='mono'>" +
        (t.label || "—") +
        "</td>" +
        "<td class='mono'>" +
        (t.url || "—") +
        "</td>" +
        "<td>" +
        (t.description || "—") +
        (t.source ? "<div class='muted small'>source: " + t.source + "</div>" : "") +
        (t.inclusion_reason
          ? "<div class='muted small'>inclusion_reason: " +
            t.inclusion_reason +
            "</div>"
          : "") +
        "</td>";
      targetsEl.appendChild(row);
    });
  }

  function renderPolicy(status) {
    if (policyAllowPlaintextEl) {
      policyAllowPlaintextEl.textContent = String(!!status.allow_plaintext_http);
    }
    if (policyMaxResponseEl) {
      policyMaxResponseEl.textContent = formatBytes(status.max_response_bytes);
    }
    if (policyMaxRedirectsEl) {
      policyMaxRedirectsEl.textContent =
        status.max_redirects == null ? "—" : String(status.max_redirects);
    }
  }

  function renderCalls(calls) {
    callsEl.textContent = "";
    if (!calls || calls.length === 0) {
      const empty = document.createElement("div");
      empty.className = "muted small";
      empty.textContent = "No recent calls.";
      callsEl.appendChild(empty);
      return;
    }

    calls.forEach((call) => {
      const key = callKey(call);
      const wrapper = document.createElement("div");
      wrapper.className = "harpoon-call";

      const header = document.createElement("div");
      header.className = "harpoon-call-header";

      const toggle = document.createElement("button");
      toggle.className = "harpoon-toggle";
      toggle.textContent = "+";

      const error = call.error || "";
      const statusText = call.status ? call.status.toString() : "—";
      const latency = call.latency_ms != null ? call.latency_ms + "ms" : "—";
      const reqBytes = formatBytes(call.req_bytes);
      const respBytes = formatBytes(call.resp_bytes);

      const timestamp = call.timestamp
        ? new Date(call.timestamp).toISOString()
        : "—";

      header.appendChild(toggle);
      header.appendChild(buildCell(timestamp, "mono small"));
      header.appendChild(buildCell(call.label || "—", "mono small"));
      header.appendChild(buildCell(call.method || "—", "mono small"));
      header.appendChild(buildCell(statusText, "mono small"));
      header.appendChild(buildCell(latency, "mono small"));
      header.appendChild(buildCell(reqBytes + " / " + respBytes, "mono small"));
      header.appendChild(buildCell(error || "—", "small"));

      const details = document.createElement("div");
      details.className = "harpoon-call-details";
      const isOpen = openCalls.has(key);
      details.style.display = isOpen ? "block" : "none";
      toggle.textContent = isOpen ? "–" : "+";

      if (!captureEnabled) {
        const note = document.createElement("div");
        note.className = "muted small";
        note.textContent = "Payload capture is disabled.";
        details.appendChild(note);
      } else {
        const reqBlock = document.createElement("div");
        reqBlock.className = "harpoon-payload";
        const reqTitle = document.createElement("div");
        reqTitle.className = "muted small";
        reqTitle.textContent = "Request body";
        const reqPre = document.createElement("pre");
        reqPre.className = "pre mono";
        reqPre.textContent = formatPayload(call.request_body, false);
        reqBlock.appendChild(reqTitle);
        reqBlock.appendChild(reqPre);

        const respBlock = document.createElement("div");
        respBlock.className = "harpoon-payload";
        const respTitle = document.createElement("div");
        respTitle.className = "muted small harpoon-response-title";
        respTitle.textContent = "Response body";
        const respToggle = document.createElement("button");
        respToggle.className = "harpoon-toggle-view";
        const hasTransformed = !!call.response_body_transformed;
        const showTransformed = hasTransformed
          ? responseView.get(key) !== false
          : false;
        if (hasTransformed) {
          respToggle.textContent = showTransformed
            ? "Show original"
            : "Show transformed";
          respToggle.addEventListener("click", () => {
            responseView.set(key, !showTransformed);
            renderCalls(calls);
          });
        } else {
          respToggle.textContent = "Original";
          respToggle.disabled = true;
        }
        respTitle.appendChild(respToggle);
        const respPre = document.createElement("pre");
        respPre.className = "pre mono";
        const respBody = hasTransformed
          ? showTransformed
            ? call.response_body_transformed
            : call.response_body
          : call.response_body;
        const respIsBase64 = hasTransformed && showTransformed
          ? false
          : call.body_is_base64;
        respPre.textContent = formatPayload(respBody, respIsBase64);
        respBlock.appendChild(respTitle);
        respBlock.appendChild(respPre);

        details.appendChild(reqBlock);
        details.appendChild(respBlock);
      }

      toggle.addEventListener("click", () => {
        const open = details.style.display === "block";
        details.style.display = open ? "none" : "block";
        toggle.textContent = open ? "+" : "–";
        if (open) {
          openCalls.delete(key);
        } else {
          openCalls.add(key);
        }
      });

      wrapper.appendChild(header);
      wrapper.appendChild(details);
      callsEl.appendChild(wrapper);
    });
  }

  function callKey(call) {
    return [
      call.timestamp || "",
      call.label || "",
      call.method || "",
      call.url || "",
      call.status || "",
    ].join("|");
  }

  function buildCell(text, cls) {
    const el = document.createElement("div");
    if (cls) el.className = cls;
    el.textContent = text;
    return el;
  }

  function updateLabelFilter(labels, current) {
    labelFilterEl.textContent = "";
    const all = document.createElement("option");
    all.value = "";
    all.textContent = "all labels";
    labelFilterEl.appendChild(all);
    labels.forEach((label) => {
      const opt = document.createElement("option");
      opt.value = label;
      opt.textContent = label;
      labelFilterEl.appendChild(opt);
    });
    labelFilterEl.value = current || "";
  }

  async function refreshStatus() {
    try {
      const status = await adminUI.fetchJSON("/api/harpoon/status");
      captureEnabled = !!status.capture_payloads;
      setBadge(
        statusBadge,
        status.enabled ? "ok" : "warn",
        "Harpoon: " + (status.enabled ? "enabled" : "disabled")
      );
      setBadge(
        captureBadge,
        captureEnabled ? "warn" : "ok",
        "Capture req/resp payloads: " + (captureEnabled ? "on" : "off")
      );
      captureBadge.title = captureEnabled
        ? "Request/response bodies are stored in call history (debug only)."
        : "Request/response bodies are not stored. Enable with --harpoon-capture-payloads.";
      setDisabledState(!status.enabled, status.reason);
      renderPolicy(status);
      return status.enabled;
    } catch (e) {
      setBadge(statusBadge, "bad", "Harpoon: error");
      setDisabledState(true, "Harpoon unavailable");
      throw e;
    }
  }

  async function refreshTargets() {
    const data = await adminUI.fetchJSON("/api/harpoon/targets");
    const targets = data.targets || [];
    renderTargets(targets);
    availableLabels = targets.map((t) => t.label).filter(Boolean);
    updateLabelFilter(availableLabels, labelFilterEl.value);
  }

  async function refreshCalls() {
    const label = labelFilterEl.value || "";
    const query = label ? "?label=" + encodeURIComponent(label) : "";
    const data = await adminUI.fetchJSON("/api/harpoon/calls" + query);
    renderCalls(data.calls || []);
  }

  async function refreshAll() {
    errEl.textContent = "";
    try {
      const enabled = await refreshStatus();
      if (!enabled) return;
      await refreshTargets();
      await refreshCalls();
    } catch (e) {
      errEl.textContent = "error: " + e;
    }
  }

  function startPolling() {
    if (poller) return;
    poller = setInterval(() => {
      if (active) refreshAll();
    }, 5000);
  }

  function stopPolling() {
    if (poller) {
      clearInterval(poller);
      poller = null;
    }
  }

  function setActiveTab(name) {
    const next = name === "harpoon";
    if (next === active) return;
    active = next;
    if (active) {
      refreshAll();
      startPolling();
    } else {
      stopPolling();
    }
  }

  labelFilterEl.addEventListener("change", refreshCalls);
  refreshBtn.addEventListener("click", refreshAll);

  if (adminUI.onTabChange) {
    adminUI.onTabChange(setActiveTab);
  }

  const selected = document.querySelector(".tab[aria-selected='true']");
  if (selected && selected.dataset && selected.dataset.tab) {
    setActiveTab(selected.dataset.tab);
  }
})();
