/*
 * Frontend for the timelapse service.
 *
 * No framework, no build step: the file is served as an ES module exactly as it
 * is committed. Everything it needs comes from the JSON API on the same origin,
 * which keeps the content security policy restricted to 'self'.
 */

"use strict";

// The running build, published by the server on the templated index.html.
// Reading it from there keeps this file byte-identical across releases, so it
// stays cacheable, while still letting a long-lived tab notice an update.
const VERSION = document.documentElement.dataset.version ?? "";

// Path the app is served under. Empty normally; under Home Assistant ingress
// it is the Supervisor's generated prefix, so every request has to carry it.
const BASE = document.documentElement.dataset.base ?? "";

/** Builds a same-origin URL that works both standalone and behind ingress. */
const url = (path) => BASE + path;
const STORAGE = {
  theme: "timelapse.theme",
  lang: "timelapse.lang",
  view: "timelapse.view",
};

/** Current translation dictionary, flattened to dotted keys. */
let messages = {};
let status = null;

/* ------------------------------------------------------- error reporting */

/**
 * Sends a frontend failure to the server so it lands in the service log.
 *
 * Someone running this in a container has the log and nothing else; a browser
 * console they never open is not a diagnostic. Failures are reported at most
 * once every few seconds so a repeating fault cannot flood anything.
 */
let lastReportAt = 0;

function reportError(context, error) {
  const detail = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
  console.error(context, error);

  const now = Date.now();
  if (now - lastReportAt < 5000) return;
  lastReportAt = now;

  try {
    void fetch(url("/api/client-error"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ context, detail, page: location.pathname }),
      keepalive: true,
    }).catch(() => {});
  } catch {
    /* Reporting must never itself break the page. */
  }
}

/* ------------------------------------------------------------------ helpers */

const $ = (id) => document.getElementById(id);

/** Reads a value from localStorage, tolerating blocked or full storage. */
function readStored(key) {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key, value) {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    /* Preferences simply do not persist; the UI still works. */
  }
}

async function getJSON(path) {
  const response = await fetch(url(path), { headers: { Accept: "application/json" } });
  if (!response.ok) {
    throw new Error(`${path} responded ${response.status}`);
  }
  return response.json();
}

/** Looks up a dotted translation key, falling back to the key itself. */
function t(key, fallback) {
  return Object.hasOwn(messages, key) ? messages[key] : (fallback ?? key);
}

/** Flattens a nested translation object into dotted keys. */
function flatten(source, prefix = "", target = {}) {
  for (const [key, value] of Object.entries(source)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === "object" && !Array.isArray(value)) {
      flatten(value, path, target);
    } else {
      target[path] = String(value);
    }
  }
  return target;
}

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** exponent;
  return `${value.toFixed(exponent === 0 ? 0 : 1)} ${units[exponent]}`;
}

function formatDateTime(iso) {
  if (!iso) return "—";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat(currentLang(), {
    dateStyle: "medium",
    timeStyle: "medium",
  }).format(date);
}

/** Renders a coarse relative age, e.g. "3 min ago". */
function formatAge(iso) {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";

  const seconds = Math.round((then - Date.now()) / 1000);
  const formatter = new Intl.RelativeTimeFormat(currentLang(), { numeric: "auto" });
  const steps = [
    [60, "second", 1],
    [3600, "minute", 60],
    [86400, "hour", 3600],
    [Infinity, "day", 86400],
  ];
  for (const [limit, unit, divisor] of steps) {
    if (Math.abs(seconds) < limit) {
      return formatter.format(Math.round(seconds / divisor), unit);
    }
  }
  return "—";
}

/* --------------------------------------------------------------------- i18n */

function currentLang() {
  return document.documentElement.lang || "en";
}

/** Picks the best language: stored choice, then browser preference, then default. */
function pickLanguage(available, fallback) {
  const stored = readStored(STORAGE.lang);
  if (stored && available.includes(stored)) return stored;

  for (const tag of navigator.languages ?? []) {
    const base = tag.toLowerCase().split("-")[0];
    if (available.includes(base)) return base;
  }
  return available.includes(fallback) ? fallback : available[0] ?? "en";
}

async function applyLanguage(lang) {
  const data = await getJSON(`/api/i18n/${encodeURIComponent(lang)}`);
  messages = flatten(data);
  document.documentElement.lang = lang;
  writeStored(STORAGE.lang, lang);
  translateDocument();
  if (status) renderStatus(status);
  renderArchiveCaption();
}

/**
 * Applies translations to every annotated element. Text is assigned through
 * textContent so a translation file can never inject markup.
 */
function translateDocument() {
  for (const el of document.querySelectorAll("[data-i18n]")) {
    el.textContent = t(el.dataset.i18n, el.textContent);
  }
  for (const el of document.querySelectorAll("[data-i18n-aria]")) {
    el.setAttribute("aria-label", t(el.dataset.i18nAria, el.getAttribute("aria-label") ?? ""));
  }
  for (const el of document.querySelectorAll("[data-i18n-alt]")) {
    el.alt = t(el.dataset.i18nAlt, el.alt);
  }
  document.title = t("app.title", "Timelapse");
}

/* -------------------------------------------------------------------- theme */

function applyTheme(theme) {
  document.documentElement.setAttribute("data-theme", theme);
  writeStored(STORAGE.theme, theme);
  const icons = { auto: "◐", light: "☀", dark: "☾" };
  $("theme-icon").textContent = icons[theme] ?? "◐";
}

function cycleTheme() {
  const order = ["auto", "light", "dark"];
  const current = document.documentElement.getAttribute("data-theme") ?? "auto";
  applyTheme(order[(order.indexOf(current) + 1) % order.length]);
}

/* --------------------------------------------------------------------- tabs */

function showView(name) {
  for (const tab of document.querySelectorAll(".tab")) {
    tab.setAttribute("aria-selected", String(tab.dataset.view === name));
  }
  for (const view of document.querySelectorAll(".view")) {
    view.hidden = view.id !== `view-${name}`;
  }
  writeStored(STORAGE.view, name);

  if (name === "archive" && !archive.loaded) {
    void initArchive();
  }
}

/* --------------------------------------------------------------- live view */

/** Shows an image, replacing the empty-state placeholder. */
function showImage(src) {
  const image = $("live-image");
  image.src = src;
  image.hidden = false;
  $("live-placeholder").hidden = true;
}

/** Shows the empty state, with an explanation of why there is no image. */
function showPlaceholder(messageKey) {
  const image = $("live-image");
  image.hidden = true;
  image.removeAttribute("src");
  const placeholder = $("live-placeholder");
  placeholder.textContent = t(messageKey, "No image available yet.");
  placeholder.hidden = false;
}

/** Reloads the last captured frame. A cache-busting parameter is required
 *  because the file is replaced in place on every capture. */
async function reloadLatest() {
  // Fetched rather than assigned to img.src so a missing image becomes an
  // explanation instead of a broken image icon.
  try {
    const response = await fetch(url(`/api/latest.jpg?t=${Date.now()}`));
    if (response.status === 404) {
      showPlaceholder(archiveHint());
      setLiveBadge("live.badgeNoImage", "warn");
      renderLiveCaption(null, null);
      return;
    }
    if (!response.ok) throw new Error(`latest image responded ${response.status}`);

    const blob = await response.blob();
    const image = $("live-image");
    if (image.dataset.objectUrl) URL.revokeObjectURL(image.dataset.objectUrl);
    const objectUrl = URL.createObjectURL(blob);
    image.dataset.objectUrl = objectUrl;
    showImage(objectUrl);

    setLiveBadge("live.badgeArchived", "ok");
    const captured = status?.capture?.latestCapturedAt ?? status?.monitor?.archiveNewestAt;
    const filename = status?.capture?.latestFilename || status?.monitor?.archiveNewest;
    renderLiveCaption(captured, filename);
  } catch (error) {
    showPlaceholder("live.noImage");
    setLiveBadge("live.badgeError", "error");
    reportError("loading the last capture failed", error);
  }
}

/** Picks the placeholder text that explains why the archive has no image. */
function archiveHint() {
  switch (status?.monitor?.archiveState) {
    case "missing":
      return "live.archiveMissing";
    case "unreadable":
      return "live.archiveUnreadable";
    case "empty":
      return "live.archiveEmpty";
    default:
      return "live.noImage";
  }
}

async function loadPreview() {
  const button = $("btn-preview");
  button.disabled = true;
  setLiveBadge("live.badgeLoading", null);
  try {
    const response = await fetch(url(`/api/live.jpg?t=${Date.now()}`));
    if (!response.ok) {
      throw new Error(`live preview responded ${response.status}`);
    }

    const blob = await response.blob();
    const image = $("live-image");
    // Release the previous object URL so repeated previews do not leak. Note
    // the name: a local `url` here would shadow the helper above for the whole
    // function and make the fetch throw before it ever runs.
    if (image.dataset.objectUrl) URL.revokeObjectURL(image.dataset.objectUrl);
    const objectUrl = URL.createObjectURL(blob);
    image.dataset.objectUrl = objectUrl;
    showImage(objectUrl);

    setLiveBadge("live.badgePreview", "warn");
    renderLiveCaption(new Date().toISOString(), t("live.notSaved", "not saved"));
  } catch (error) {
    setLiveBadge("live.badgeError", "error");
    // Without this the failure is visible only in the browser console, which
    // is not where anyone running this looks.
    reportError("live preview failed", error);
  } finally {
    button.disabled = false;
  }
}

function setLiveBadge(key, tone) {
  const badge = $("live-badge");
  badge.hidden = false;
  badge.textContent = t(key, "");
  if (tone) {
    badge.dataset.tone = tone;
  } else {
    delete badge.dataset.tone;
  }
}

function renderLiveCaption(iso, detail) {
  const parts = [];
  if (iso) parts.push(formatDateTime(iso));
  if (detail) parts.push(detail);
  $("live-caption").textContent = parts.join(" · ");
}

/* ------------------------------------------------------------------ archive */

const archive = {
  loaded: false,
  frames: [],
  filtered: [],
  index: 0,
  timer: null,
  playing: false,
};

async function initArchive() {
  archive.loaded = true;
  try {
    const { years } = await getJSON("/api/archive/years");
    fillSelect($("sel-year"), years);
    if (years.length > 0) await loadMonths(years[0]);
  } catch {
    $("archive-empty").hidden = false;
  }
}

async function loadMonths(year) {
  const { months } = await getJSON(`/api/archive/years/${encodeURIComponent(year)}/months`);
  fillSelect($("sel-month"), months);
  if (months.length > 0) await loadDays(months[0]);
}

async function loadDays(month) {
  const { days } = await getJSON(`/api/archive/months/${encodeURIComponent(month)}/days`);
  fillSelect($("sel-day"), days);
  if (days.length > 0) await loadFrames(days[0]);
}

async function loadFrames(day) {
  stopPlayback();
  const { frames } = await getJSON(`/api/archive/days/${encodeURIComponent(day)}/frames`);
  archive.frames = frames.map((frame) => ({ ...frame, day }));

  // Images captured by different sources over the years share a day directory;
  // mixing them in one playback would make the timelapse jump between framings.
  const groups = [...new Set(archive.frames.map((frame) => frame.group))].filter(Boolean);
  const groupField = $("group-field");
  const groupSelect = $("sel-group");
  if (groups.length > 1) {
    fillSelect(groupSelect, groups);
    groupField.hidden = false;
  } else {
    groupField.hidden = true;
    groupSelect.innerHTML = "";
  }

  applyGroupFilter();
}

/** Fetches the completeness report and updates the export links for a day. */
async function loadReport(day) {
  const group = $("group-field").hidden ? "" : $("sel-group").value;
  const query = group ? `?group=${encodeURIComponent(group)}` : "";

  const video = $("btn-video");
  const zip = $("btn-zip");
  video.hidden = !(status?.monitor?.videoAvailable);
  zip.hidden = !(status?.config?.zipEnabled);
  video.href = url(`/api/archive/days/${encodeURIComponent(day)}/video.mp4${query}`);
  zip.href = url(`/api/archive/days/${encodeURIComponent(day)}/download.zip${query}`);

  const element = $("archive-report");
  try {
    const report = await getJSON(`/api/archive/days/${encodeURIComponent(day)}/report${query}`);
    const parts = [`${report.frames} ${t("archive.frames", "frames")}`];

    if (report.gaps.length > 0) {
      parts.push(
        `${report.gaps.length} ${t("archive.gaps", "gaps")}`,
        `${report.missingFrames} ${t("archive.missing", "missing")}`,
      );
      // Name the worst offenders so a gap is actionable rather than a number.
      const worst = [...report.gaps]
        .sort((a, b) => b.seconds - a.seconds)
        .slice(0, 3)
        .map((gap) => `${gap.after}\u2009\u2192\u2009${gap.before}`)
        .join(", ");
      parts.push(worst);
      element.dataset.tone = "warn";
    } else {
      delete element.dataset.tone;
    }

    element.textContent = parts.join(" \u00b7 ");
    element.hidden = false;
  } catch {
    element.hidden = true;
  }
}

function applyGroupFilter() {
  const groupSelect = $("sel-group");
  const group = $("group-field").hidden ? null : groupSelect.value;
  archive.filtered = group
    ? archive.frames.filter((frame) => frame.group === group)
    : archive.frames;

  archive.index = 0;
  const scrubber = $("scrubber");
  scrubber.max = String(Math.max(archive.filtered.length - 1, 0));
  scrubber.value = "0";

  const hasFrames = archive.filtered.length > 0;
  $("archive-viewer").hidden = !hasFrames;
  $("archive-player").hidden = !hasFrames;
  $("archive-empty").hidden = hasFrames;

  if (hasFrames) {
    showFrame(0);
    void loadReport(archive.filtered[0].day);
  }
}

function showFrame(index) {
  if (archive.filtered.length === 0) return;
  archive.index = Math.max(0, Math.min(index, archive.filtered.length - 1));

  const frame = archive.filtered[archive.index];
  $("archive-image").src =
    url(`/api/archive/days/${encodeURIComponent(frame.day)}/frames/${encodeURIComponent(frame.name)}`);
  $("scrubber").value = String(archive.index);
  renderArchiveCaption();
}

function renderArchiveCaption() {
  if (archive.filtered.length === 0) {
    $("archive-caption").textContent = "";
    return;
  }
  const frame = archive.filtered[archive.index];
  const position = `${archive.index + 1} / ${archive.filtered.length}`;
  $("archive-caption").textContent =
    [frame.day, frame.time, position, formatBytes(frame.bytes)].filter(Boolean).join(" · ");
}

function togglePlayback() {
  if (archive.playing) {
    stopPlayback();
  } else {
    startPlayback();
  }
}

function startPlayback() {
  if (archive.filtered.length < 2) return;
  archive.playing = true;
  $("btn-play").textContent = "⏸";

  const delay = Number($("sel-speed").value) || 200;
  archive.timer = window.setInterval(() => {
    if (archive.index >= archive.filtered.length - 1) {
      stopPlayback();
      return;
    }
    showFrame(archive.index + 1);
  }, delay);
}

function stopPlayback() {
  archive.playing = false;
  $("btn-play").textContent = "▶";
  if (archive.timer !== null) {
    window.clearInterval(archive.timer);
    archive.timer = null;
  }
}

function fillSelect(select, values) {
  select.innerHTML = "";
  for (const value of values) {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = value;
    select.append(option);
  }
}

/* ------------------------------------------------------------------- status */

async function refreshStatus() {
  try {
    status = await getJSON("/api/status");
  } catch {
    return;
  }
  renderStatus(status);

  // The running binary changed underneath a long-lived tab: offer a reload so
  // no stale asset stays in use.
  if (status.version && VERSION && status.version !== VERSION) {
    $("update-toast").hidden = false;
  }
}

/** Builds a definition list. Values are set via textContent, never innerHTML. */
function renderFacts(container, rows) {
  container.replaceChildren();
  for (const row of rows) {
    if (row.value === undefined || row.value === null || row.value === "") continue;
    const dt = document.createElement("dt");
    dt.textContent = t(row.key, row.key);
    const dd = document.createElement("dd");
    dd.textContent = String(row.value);
    if (row.tone) dd.dataset.tone = row.tone;
    container.append(dt, dd);
  }
}

function renderStatus(data) {
  const capture = data.capture;
  const sync = data.sync;
  const config = data.config;

  const healthy = data.health.healthy;
  const badge = $("status-badge");
  badge.textContent = healthy ? t("status.ok", "Healthy") : t("status.problem", "Attention");
  badge.dataset.tone = healthy ? "ok" : "error";
  badge.title = data.health.reason ?? "";

  // A deployment that does not capture has no capture facts worth showing;
  // listing them all as dashes would only bury the ones that matter.
  const capturing = config.captureEnabled;
  const captureFacts = capturing
    ? [
        { key: "status.captureActive", value: yesNo(capture.active), tone: capture.active ? "ok" : null },
        { key: "status.window", value: `${capture.windowStart} – ${capture.windowEnd}` },
        { key: "status.nextCapture", value: formatDateTime(capture.nextCapture) },
        { key: "status.lastSuccess", value: `${formatDateTime(capture.lastSuccess)} (${formatAge(capture.lastSuccess)})` },
        { key: "status.lastFile", value: capture.latestFilename },
        { key: "status.lastSize", value: capture.latestBytes ? formatBytes(capture.latestBytes) : null },
        {
          key: "status.failures",
          value: capture.consecutiveFailures,
          tone: capture.consecutiveFailures > 0 ? "error" : null,
        },
        { key: "status.lastError", value: capture.lastError, tone: "error" },
        { key: "status.totals", value: `${capture.totalOk} / ${capture.totalFailed}` },
      ]
    : [
        { key: "status.role", value: t("status.roleWatchdog", "watchdog (not capturing)") },
        { key: "status.window", value: `${capture.windowStart} – ${capture.windowEnd}` },
      ];

  renderFacts($("status-facts"), [
    ...captureFacts,
    { key: "status.health", value: data.health.reason, tone: healthy ? null : "error" },
    {
      key: "status.archiveAvailable",
      value: sync.enabled || sync.delegated
        ? yesNo(sync.archiveAvailable)
        : t("status.syncDisabled", "sync disabled"),
      tone: !(sync.enabled || sync.delegated) ? null : sync.archiveAvailable ? "ok" : "error",
    },
    {
      key: "status.archiveService",
      value: sync.delegated ? yesNo(sync.serviceReachable) : null,
      tone: sync.delegated && !sync.serviceReachable ? "error" : null,
    },
    { key: "status.nextSync", value: formatDateTime(sync.nextSync) },
    { key: "status.lastSync", value: `${formatDateTime(sync.lastSuccess)} (${formatAge(sync.lastSuccess)})` },
    { key: "status.lastSyncFiles", value: `${sync.lastFiles} · ${formatBytes(sync.lastBytes)}` },
    { key: "status.syncError", value: sync.lastError, tone: "error" },
    {
      key: "status.spool",
      value: `${sync.spoolFiles} · ${formatBytes(sync.spoolBytes)}`,
      tone: sync.spoolFiles > 0 && !sync.archiveAvailable ? "warn" : null,
    },
    { key: "status.spoolOldest", value: sync.spoolOldest ? formatDateTime(sync.spoolOldest) : null },
    { key: "status.uptime", value: data.uptime },
  ]);

  const monitor = data.monitor;
  const showMonitor = monitor.enabled || monitor.archiveNewestAt || capture.active;
  $("monitor-card").hidden = !showMonitor;

  if (showMonitor) {
    const monitorOk = monitor.cameraOnline && !monitor.archiveStale && !monitor.frameFrozen;
    const monitorBadge = $("monitor-badge");
    monitorBadge.textContent = monitorOk ? t("status.ok", "Healthy") : t("status.problem", "Attention");
    monitorBadge.dataset.tone = monitorOk ? "ok" : "error";

    renderFacts($("monitor-facts"), [
      {
        key: "monitor.cameraOnline",
        value: monitor.lastProbe || capture.lastSuccess ? yesNo(monitor.cameraOnline) : "—",
        tone: monitor.cameraOnline ? "ok" : "error",
      },
      { key: "monitor.lastProbe", value: monitor.lastProbe ? `${formatDateTime(monitor.lastProbe)} (${formatAge(monitor.lastProbe)})` : null },
      { key: "monitor.probeError", value: monitor.lastProbeError, tone: "error" },
      {
        key: "monitor.sourceInUse",
        value: monitor.sourceInUse ? t(`monitor.source_${monitor.sourceInUse}`, monitor.sourceInUse) : null,
        tone: monitor.sourceInUse === "fallback" ? "warn" : null,
      },
      {
        key: "monitor.archiveNewest",
        value: monitor.archiveNewestAt
          ? `${formatDateTime(monitor.archiveNewestAt)} (${formatAge(monitor.archiveNewestAt)})`
          : "—",
        tone: monitor.archiveStale ? "error" : null,
      },
      { key: "monitor.archiveNewestFile", value: monitor.archiveNewest },
      {
        key: "monitor.archiveState",
        value: monitor.archiveState
          ? t(`monitor.archive_${monitor.archiveState}`, monitor.archiveState)
          : null,
        tone: monitor.archiveState && monitor.archiveState !== "ok" ? "error" : "ok",
      },
      { key: "monitor.archiveError", value: monitor.archiveError, tone: "error" },
      { key: "monitor.networkConflict", value: monitor.networkConflict, tone: "error" },
      { key: "monitor.archiveMaxAge", value: monitor.archiveMaxAge },
      {
        key: "monitor.frameFrozen",
        value: yesNo(monitor.frameFrozen),
        tone: monitor.frameFrozen ? "error" : null,
      },
      {
        key: "monitor.identicalFrames",
        value: monitor.identicalFrames,
        tone: monitor.identicalFrames > 0 ? "warn" : null,
      },
    ]);
  }

  renderFacts($("config-facts"), [
    { key: "config.cameraSource", value: config.cameraSource },
    { key: "config.cameraFallback", value: config.cameraFallback || t("config.noFallback", "none") },
    { key: "config.cameraTarget", value: config.cameraTarget },
    { key: "config.protectKey", value: config.cameraSource === "protect" ? yesNo(config.protectKeySet) : null },
    {
      key: "config.protectInsecure",
      value: config.cameraSource === "protect" ? yesNo(config.protectInsecureTls) : null,
      tone: config.protectInsecureTls ? "warn" : null,
    },
    { key: "config.interval", value: config.captureInterval },
    { key: "config.scheduleMode", value: config.scheduleMode },
    { key: "config.activeWindow", value: config.activeWindow },
    { key: "config.prefix", value: config.filenamePrefix },
    { key: "config.timezone", value: config.timezone },
    { key: "config.spoolDir", value: config.spoolDir },
    { key: "config.archiveDir", value: config.archiveDir || t("status.syncDisabled", "sync disabled") },
    { key: "config.sentinel", value: config.archiveSentinel },
    { key: "config.syncMode", value: config.syncMode },
    { key: "config.syncAt", value: config.syncMode === "off" ? null : config.syncAt },
    { key: "config.authMode", value: config.authMode },
    { key: "config.monitor", value: yesNo(config.monitorEnabled) },
    { key: "config.video", value: yesNo(data.monitor.videoAvailable) },
    { key: "config.haPublish", value: yesNo(config.haPublish) },
    { key: "config.metrics", value: yesNo(config.metricsEnabled) },
    { key: "config.notify", value: yesNo(config.notifyEnabled) },
    { key: "config.version", value: data.version },
  ]);

  $("footer-version").textContent = data.version;
  if (config.siteName) {
    $("brand-subtitle").textContent = config.siteName;
  }

  if (!$("live-image").src && !$("live-image").dataset.checked) {
    $("live-image").dataset.checked = "1";
    void reloadLatest();
  }
}

function yesNo(value) {
  return value ? t("common.yes", "yes") : t("common.no", "no");
}

/* --------------------------------------------------------------------- boot */

async function main() {
  applyTheme(readStored(STORAGE.theme) ?? "auto");
  $("theme-toggle").addEventListener("click", cycleTheme);

  // Language list first, so the UI is never rendered in the wrong language.
  let languages = ["en"];
  let fallback = "en";
  try {
    const payload = await getJSON("/api/languages");
    languages = payload.languages ?? languages;
    fallback = payload.default ?? fallback;
  } catch {
    /* Keep the built-in default. */
  }

  const select = $("lang-select");
  fillSelect(select, languages);
  const chosen = pickLanguage(languages, fallback);
  select.value = chosen;
  select.addEventListener("change", () => void applyLanguage(select.value));
  await applyLanguage(chosen);

  for (const tab of document.querySelectorAll(".tab")) {
    tab.addEventListener("click", () => showView(tab.dataset.view));
  }
  showView(readStored(STORAGE.view) ?? "live");

  $("btn-reload").addEventListener("click", () => void reloadLatest());
  $("btn-preview").addEventListener("click", () => void loadPreview());

  $("sel-year").addEventListener("change", (event) => void loadMonths(event.target.value));
  $("sel-month").addEventListener("change", (event) => void loadDays(event.target.value));
  $("sel-day").addEventListener("change", (event) => void loadFrames(event.target.value));
  $("sel-group").addEventListener("change", applyGroupFilter);
  $("sel-speed").addEventListener("change", () => {
    if (archive.playing) {
      stopPlayback();
      startPlayback();
    }
  });

  $("btn-play").addEventListener("click", togglePlayback);
  $("btn-first").addEventListener("click", () => { stopPlayback(); showFrame(0); });
  $("btn-last").addEventListener("click", () => { stopPlayback(); showFrame(archive.filtered.length - 1); });
  $("btn-prev").addEventListener("click", () => { stopPlayback(); showFrame(archive.index - 1); });
  $("btn-next").addEventListener("click", () => { stopPlayback(); showFrame(archive.index + 1); });
  $("scrubber").addEventListener("input", (event) => {
    stopPlayback();
    showFrame(Number(event.target.value));
  });

  $("btn-update").addEventListener("click", () => window.location.reload());

  await refreshStatus();
  window.setInterval(() => void refreshStatus(), 30_000);

  // Refresh the archived frame shortly after each expected capture.
  window.setInterval(() => {
    if (!$("view-live").hidden && !status?.capture?.active) return;
    if (!$("view-live").hidden) void reloadLatest();
  }, 60_000);

  window.addEventListener("error", (event) => {
    reportError("uncaught error", event.error ?? event.message);
  });
  window.addEventListener("unhandledrejection", (event) => {
    reportError("unhandled promise rejection", event.reason);
  });

  registerServiceWorker();
}

function registerServiceWorker() {
  if (!("serviceWorker" in navigator)) return;
  // Under Home Assistant ingress the app lives on a generated sub-path that
  // changes between sessions, so a worker scoped to it would be useless and
  // its cache would go stale immediately.
  if (BASE !== "") return;
  navigator.serviceWorker.register("/sw.js").then((registration) => {
    registration.addEventListener("updatefound", () => {
      const worker = registration.installing;
      if (!worker) return;
      worker.addEventListener("statechange", () => {
        // A new worker took over while the page was open: the assets on screen
        // are now the previous release.
        if (worker.state === "installed" && navigator.serviceWorker.controller) {
          $("update-toast").hidden = false;
        }
      });
    });
  }).catch(() => {
    /* The app works without offline support. */
  });
}

void main();
