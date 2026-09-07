/*
 * Service worker.
 *
 * The cache name carries the build version, so every release starts with an
 * empty cache and the activate handler deletes everything belonging to older
 * versions. That is what guarantees no stale CSS or JavaScript survives an
 * update.
 *
 * Only the application shell is cached. Images and API responses are always
 * fetched from the network: a cached snapshot would be actively misleading.
 */

"use strict";

const VERSION = "__VERSION__";
const CACHE = `timelapse-shell-${VERSION}`;

const SHELL = [
  "/",
  `/static/app.css?v=${VERSION}`,
  `/static/app.js?v=${VERSION}`,
  `/static/theme.js?v=${VERSION}`,
  `/static/icon.svg?v=${VERSION}`,
  "/manifest.webmanifest",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      // A missing asset must not block installation of the worker.
      .then((cache) => cache.addAll(SHELL).catch(() => undefined))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET") return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // Never serve a stale image or a stale status from cache.
  if (url.pathname.startsWith("/api/") || url.pathname === "/metrics" || url.pathname === "/healthz") {
    return;
  }

  // Navigations go to the network first so a new release is picked up as soon
  // as it is reachable, with the cached shell as the offline fallback.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(() => caches.match("/", { ignoreSearch: false }).then((hit) => hit ?? Response.error())),
    );
    return;
  }

  event.respondWith(
    caches.match(request).then((hit) => {
      if (hit) return hit;
      return fetch(request).then((response) => {
        // Only versioned assets are worth storing; anything else would risk
        // outliving its release.
        if (response.ok && url.searchParams.get("v") === VERSION) {
          const copy = response.clone();
          void caches.open(CACHE).then((cache) => cache.put(request, copy));
        }
        return response;
      });
    }),
  );
});
