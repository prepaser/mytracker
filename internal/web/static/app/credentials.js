(function () {
  let revealTimer = null;
  document.querySelectorAll('.owntracks-reveal').forEach(button => {
    button.addEventListener('click', () => window.mytrackerUI.withButton(button, async () => {
      const cfg = JSON.parse(document.getElementById('cfg').textContent);
      const response = await window.mytrackerUI.request('/api/owntracks-url', {
        method: 'POST',
        headers: { 'X-CSRF-Token': cfg.csrf, 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ csrf: cfg.csrf })
      });
      const out = document.getElementById('owntracks-url');
      out.textContent = response.url;
      out.setAttribute('aria-label', 'OwnTracks URL revealed');
      clearTimeout(revealTimer);
      revealTimer = setTimeout(() => {
        out.textContent = 'Hidden';
        out.setAttribute('aria-label', 'OwnTracks URL hidden');
      }, 30000);
      const copied = await window.mytrackerUI.copy(response.url);
      window.mytrackerUI.toast(copied ? 'OwnTracks URL copied.' : 'OwnTracks URL revealed. Copy it manually before it hides.', copied ? 'success' : 'warning');
    }, 'Revealing').catch(error => window.mytrackerUI.toast(error.message, 'error')));
  });
})();
