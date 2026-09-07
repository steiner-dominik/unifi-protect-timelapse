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
const STORAGE = {
  theme: "timelapse.theme",
  lang: "timelapse.lang",
  view: "timelapse.view",
};

/** Current translation dictionary, flattened to dotted keys. */
let messages = {};
let status = null;

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

async function getJSON(url) {
  const response = await fetch(url, { headers: { Accept: "application/json" } });
  if (!response.ok) {
    throw new Error(`${url} responded ${response.status}`);
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

/** Reloads the last captured frame. A cache-busting parameter is required
 *  because the file is replaced in place on every capture. */
function reloadLatest() {
  const image = $("live-image");
  image.src = `/api/latest.jpg?t=${Date.now()}`;
  setLiveBadge("live.badgeArchived", "ok");
  renderLiveCaption(status?.capture?.latestCapturedAt, status?.capture?.latestFilename);
}

async function loadPreview() {
  const button = $("btn-preview");
  button.disabled = true;
  setLiveBadge("live.badgeLoading", null);
  try {
    const response = await fetch(`/api/live.jpg?t=${Date.now()}`);
    if (!response.ok) throw new Error(String(response.status));

    const blob = await response.blob();
    const image = $("live-image");
    // Release the previous object URL so repeated previews do not leak.
    if (image.dataset.objectUrl) URL.revokeObjectURL(image.dataset.objectUrl);
    const url = URL.createObjectURL(blob);
    image.dataset.objectUrl = url;
    image.src = url;

    setLiveBadge("live.badgePreview", "warn");
    renderLiveCaption(new Date().toISOString(), t("live.notSaved", "not saved"));
  } catch {
    setLiveBadge("live.badgeError", "error");
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

  if (hasFrames) showFrame(0);
}

function showFrame(index) {
  if (archive.filtered.length === 0) return;
  archive.index = Math.max(0, Math.min(index, archive.filtered.length - 1));

  const frame = archive.filtered[archive.index];
  $("archive-image").src =
    `/api/archive/days/${encodeURIComponent(frame.day)}/frames/${encodeURIComponent(frame.name)}`;
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

  const healthy = capture.consecutiveFailures === 0 && Boolean(capture.lastSuccess);
  const badge = $("status-badge");
  badge.textContent = healthy ? t("status.ok", "Healthy") : t("status.problem", "Attention");
  badge.dataset.tone = healthy ? "ok" : "error";

  renderFacts($("status-facts"), [
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
    {
      key: "status.archiveAvailable",
      value: sync.enabled ? yesNo(sync.archiveAvailable) : t("status.syncDisabled", "sync disabled"),
      tone: !sync.enabled ? null : sync.archiveAvailable ? "ok" : "error",
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

  renderFacts($("config-facts"), [
    { key: "config.cameraSource", value: config.cameraSource },
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
    { key: "config.auth", value: yesNo(config.authEnabled) },
    { key: "config.metrics", value: yesNo(config.metricsEnabled) },
    { key: "config.notify", value: yesNo(config.notifyEnabled) },
    { key: "config.version", value: data.version },
  ]);

  $("footer-version").textContent = data.version;
  if (config.siteName) {
    $("brand-subtitle").textContent = config.siteName;
  }

  if (!$("live-image").src) reloadLatest();
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

  $("btn-reload").addEventListener("click", reloadLatest);
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
    if (!$("view-live").hidden && !$("live-image").dataset.objectUrl) reloadLatest();
  }, 60_000);

  registerServiceWorker();
}

function registerServiceWorker() {
  if (!("serviceWorker" in navigator)) return;
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
