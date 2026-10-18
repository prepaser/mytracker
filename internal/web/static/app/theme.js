(function () {
  let theme = null;
  try { theme = localStorage.getItem('mytracker-theme'); } catch (_) {}
  if (!['light', 'dark'].includes(theme)) {
    theme = matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  document.documentElement.dataset.theme = theme;
  document.documentElement.style.colorScheme = theme;
})();
