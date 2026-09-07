/*
 * Applies the stored theme before the page paints, so switching between light
 * and dark never flashes the wrong colours. This runs as a separate render
 * blocking script rather than an inline one, because the content security
 * policy forbids inline script.
 */
(function () {
  "use strict";

  var STORAGE_KEY = "timelapse.theme";
  var theme = "auto";

  try {
    var stored = window.localStorage.getItem(STORAGE_KEY);
    if (stored === "light" || stored === "dark" || stored === "auto") {
      theme = stored;
    }
  } catch (err) {
    /* Private browsing or blocked storage: fall back to the automatic theme. */
  }

  document.documentElement.setAttribute("data-theme", theme);
})();
