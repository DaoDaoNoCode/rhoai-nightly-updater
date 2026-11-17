/*
 * Applies the PatternFly theme classes to <html> before the first paint, so
 * the page never flashes in the wrong theme. It is a file, loaded
 * synchronously in <head> before the bundle, because the app's CSP
 * (default-src 'self') allows no inline script.
 *
 * Plain ES5 copy of src/theme/themeCore.ts (readStoredTheme, resolveTheme,
 * applyThemeClasses); src/theme/themeInit.test.ts runs this file against
 * those functions for every stored value and OS preference. Change both.
 */
(function () {
  var root = document.documentElement;
  root.classList.add('pf-v6-theme-felt');
  var scheme = null;
  var contrast = null;
  try {
    var storage = window.localStorage;
    var legacy = storage.getItem('pf-theme');
    if (legacy !== null) {
      if (storage.getItem('rhoai-updater.color-scheme') === null && (legacy === 'light' || legacy === 'dark')) {
        storage.setItem('rhoai-updater.color-scheme', legacy);
      }
      storage.removeItem('pf-theme');
    }
    scheme = storage.getItem('rhoai-updater.color-scheme');
    contrast = storage.getItem('rhoai-updater.contrast-mode');
  } catch (e) {
    // Storage disabled: follow the system.
  }
  var matches = function (query) {
    try {
      return !!(window.matchMedia && window.matchMedia(query).matches);
    } catch (e) {
      return false;
    }
  };
  var dark = scheme === 'dark' || (scheme !== 'light' && matches('(prefers-color-scheme: dark)'));
  var effective = contrast;
  if (effective !== 'default' && effective !== 'high-contrast' && effective !== 'glass') {
    effective = matches('(prefers-contrast: more)')
      ? 'high-contrast'
      : matches('(prefers-reduced-transparency: reduce)')
        ? 'default'
        : 'glass';
  }
  root.classList.toggle('pf-v6-theme-dark', dark);
  root.classList.toggle('pf-v6-theme-glass', effective === 'glass');
  root.classList.toggle('pf-v6-theme-high-contrast', effective === 'high-contrast');
})();
