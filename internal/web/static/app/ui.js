(function () {
  const ui = {};
  let paused = false;
  let csrf = '';
  const streams = new Set();
  const reads = new Set();
  const resumeListeners = new Set();
  const secretQueue = [];
  let activeSecret = null;
  let closingSecret = null;

  function icons(root) {
    if (window.lucide) window.lucide.createIcons({ attrs: { 'aria-hidden': 'true' }, root: root || document });
  }

  function theme() {
    return document.documentElement.dataset.theme || 'dark';
  }

  function applyTheme(value, persist = true) {
    document.documentElement.dataset.theme = value;
    document.documentElement.style.colorScheme = value;
    if (persist) {
      try { localStorage.setItem('mytracker-theme', value); } catch (_) {}
    }
    document.querySelectorAll('[data-theme-toggle]').forEach(button => {
      const next = value === 'dark' ? 'Light' : 'Dark';
	  const label = button.classList.contains('icon-button') ? '' : `<span>${next} theme</span>`;
	  button.innerHTML = `<i data-lucide="${value === 'dark' ? 'sun' : 'moon'}"></i>${label}`;
      button.setAttribute('aria-label', `Switch to ${next.toLowerCase()} theme`);
    });
    icons();
  }

  ui.toast = function (message, kind = 'info', persist = false) {
    if (persist) {
      try { sessionStorage.setItem('mytracker-flash', JSON.stringify({ message, kind })); } catch (_) {}
      return;
    }
    const region = document.getElementById('toast-region');
    if (!region) return;
    const toast = document.createElement('div');
    toast.className = `toast ${kind}`;
    const icon = kind === 'error' ? 'circle-alert' : kind === 'success' ? 'circle-check' : 'info';
    toast.innerHTML = `<i data-lucide="${icon}"></i><span></span><button class="icon-button" aria-label="Dismiss"><i data-lucide="x"></i></button>`;
    toast.querySelector('span').textContent = message;
    toast.querySelector('button').onclick = () => toast.remove();
    region.appendChild(toast);
    icons(toast);
    setTimeout(() => toast.remove(), kind === 'error' ? 8000 : 4500);
  };

  ui.icons = icons;
  ui.isPaused = () => paused;
  ui.isCanceled = error => error?.name === 'AbortError' || error?.code === 'session_paused';
  ui.onResume = callback => {
    resumeListeners.add(callback);
    return () => resumeListeners.delete(callback);
  };

  function pauseSession() {
    if (paused) return;
    paused = true;
    streams.forEach(stream => stream.pause());
    reads.forEach(controller => controller.abort());
    ui.setConnection('signed_out');
    const notice = document.getElementById('session-notice');
    if (notice) notice.hidden = false;
    const dialogs = [...document.querySelectorAll('dialog[open]')].filter(dialog => dialog.id !== 'ui-secret');
    dialogs.forEach(dialog => dialog.close());
    const resume = document.getElementById('session-resume');
    if (resume && dialogs.length) queueMicrotask(() => resume.focus());
  }

  ui.request = async function (url, options = {}) {
    const admin = new URL(url, location.href).origin === location.origin && new URL(url, location.href).pathname.startsWith('/api/');
    if (admin && paused) throw Object.assign(new Error('Sign in to resume'), { status: 401, code: 'session_paused' });
    const controller = new AbortController();
    const signal = options.signal ? AbortSignal.any([options.signal, controller.signal]) : controller.signal;
    const headers = new Headers(options.headers);
    if (admin && csrf && headers.has('X-CSRF-Token')) headers.set('X-CSRF-Token', csrf);
    if (admin && (!options.method || options.method === 'GET')) reads.add(controller);
    try {
      const response = await fetch(url, { ...options, headers, signal });
      const type = response.headers.get('Content-Type') || '';
      const body = type.includes('json') ? await response.json() : await response.text();
      signal.throwIfAborted();
      if (!response.ok) {
        const error = Object.assign(new Error(body?.error?.message || `Request failed (${response.status})`), {
          status: response.status, code: body?.error?.code || '',
          requestId: body?.error?.request_id || response.headers.get('X-Request-ID') || '', details: body?.error
        });
        if (admin && response.status === 401) pauseSession();
        throw error;
      }
      return body;
    } finally {
      reads.delete(controller);
    }
  };

  function listContext(button) {
    const row = button.closest('tr, [role="row"], .data-row, li');
    const parent = row?.parentElement;
    if (!row || !parent) return null;
    const selector = row.matches('tr') ? 'tr' : row.matches('[role="row"]') ? '[role="row"]' : row.matches('.data-row') ? '.data-row' : 'li';
    const index = [...parent.children].filter(node => node.matches(selector)).indexOf(row);
    return index < 0 ? null : () => {
      const rows = [...parent.children].filter(node => node.matches(selector));
      const target = rows[Math.min(index, rows.length - 1)];
      const action = target?.querySelector('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])');
      if (action) return action;
      const section = parent.closest('section, [role="tabpanel"]');
      const command = section?.querySelector('button:not([disabled]), a[href]');
      if (command) return command;
      const container = section || parent.closest('table, [role="grid"], ul, ol') || parent;
      if (!container.hasAttribute('tabindex')) container.tabIndex = -1;
      return container;
    };
  }

  ui.withButton = async function (button, task, pendingText) {
    if (!button || button.disabled) return;
    const returnFocus = document.activeElement;
    const returnFocusContext = listContext(button);
    const original = button.innerHTML;
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    if (pendingText) button.innerHTML = `<i data-lucide="loader-circle" class="spin"></i><span>${pendingText}</span>`;
    icons(button);
    try { return await task(); } finally {
      button.disabled = false;
      button.removeAttribute('aria-busy');
      button.innerHTML = original;
      icons(button);
      if (returnFocus && returnFocus !== document.body && document.activeElement === document.body && !document.querySelector('dialog[open]')) restoreSecretFocus(returnFocus, returnFocusContext?.());
    }
  };

  ui.copy = async function (value) {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(value);
        return true;
      }
    } catch (_) {}
    const previousFocus = document.activeElement;
    const fallback = document.createElement('textarea');
    fallback.className = 'sr-only';
    fallback.tabIndex = -1;
    fallback.value = value;
    fallback.setAttribute('readonly', '');
    const host = document.querySelector('dialog[open]') || document.body;
    try {
      host.appendChild(fallback);
      fallback.focus();
      fallback.select();
      return document.execCommand?.('copy') === true;
    } catch (_) {
      return false;
    } finally {
      fallback.remove();
      try { previousFocus?.focus(); } catch (_) {}
    }
  };

  function focusSecretTarget(target) {
    if (!target?.isConnected || target.disabled || target.hidden || target.getAttribute?.('aria-hidden') === 'true' || !target.getClientRects().length) return false;
    target.focus();
    return document.activeElement === target;
  }

  function restoreSecretFocus(target, fallbackTarget) {
    if (paused) {
      const notice = document.getElementById('session-notice');
      if (notice) notice.hidden = false;
      if (focusSecretTarget(document.getElementById('session-resume'))) return;
    }
    if (focusSecretTarget(target)) return;
    if (focusSecretTarget(fallbackTarget)) return;
    const fallback = [...document.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')]
      .find(element => element.isConnected && !element.hidden && element.getAttribute?.('aria-hidden') !== 'true' && element.getClientRects().length);
    focusSecretTarget(fallback);
  }

  ui.confirm = function ({ title, message, confirmLabel = 'Confirm', danger = false, requiredText = '' }) {
    const dialog = document.getElementById('ui-confirm');
    const form = document.getElementById('ui-confirm-form');
    const action = document.getElementById('ui-confirm-action');
    const inputWrap = document.getElementById('ui-confirm-input-wrap');
    const input = document.getElementById('ui-confirm-input');
    document.getElementById('ui-confirm-title').textContent = title;
    document.getElementById('ui-confirm-message').textContent = message;
    action.textContent = confirmLabel;
    action.className = danger ? 'btn-danger' : 'btn-primary';
    inputWrap.hidden = !requiredText;
    input.value = '';
    input.placeholder = requiredText;
    action.disabled = Boolean(requiredText);
    const returnFocus = document.activeElement;
    const returnFocusFallback = returnFocus?.closest?.('.table-wrap');
    return new Promise(resolve => {
      let settled = false;
      const cancelButtons = form.querySelectorAll('[data-confirm-cancel]');
      const finish = value => {
        if (settled) return;
        settled = true;
        form.removeEventListener('submit', submitted);
        cancelButtons.forEach(button => button.onclick = null);
        input.oninput = null;
        dialog.removeEventListener('close', closed);
        if (dialog.open) dialog.close();
        if (returnFocus !== document.body) restoreSecretFocus(returnFocus, returnFocusFallback);
        resolve(value);
      };
      const closed = () => finish(false);
      const submitted = event => {
        event.preventDefault();
        if (!action.disabled) finish(true);
      };
      input.oninput = () => { action.disabled = input.value !== requiredText; };
      form.addEventListener('submit', submitted);
      cancelButtons.forEach(button => button.onclick = () => finish(false));
      dialog.addEventListener('close', closed, { once: true });
      dialog.showModal();
      (requiredText ? input : action).focus();
    });
  };

  function openNextSecret() {
    if (activeSecret || closingSecret || !secretQueue.length) return;
    const item = secretQueue.shift();
    const dialog = document.getElementById('ui-secret');
    const input = document.getElementById('ui-secret-value');
    const copy = document.getElementById('ui-secret-copy');
    const title = document.getElementById('ui-secret-title');
    activeSecret = item;
    const cleanup = () => {
      dialog.querySelectorAll('[data-secret-close]').forEach(button => button.onclick = null);
      copy.onclick = null;
    };
    const finalizeClose = () => {
      if (!item.closing || closingSecret !== item) return;
      dialog.removeEventListener('close', closed);
      input.value = '';
      closingSecret = null;
      activeSecret = null;
      if (secretQueue.length) openNextSecret();
      else restoreSecretFocus(item.returnFocus, item.returnFocusFallback);
      item.resolve();
    };
    const finish = () => {
      if (item.done || item.closing) return;
      item.done = true;
      cleanup();
      if (secretQueue.length) {
        dialog.removeEventListener('close', closed);
        activeSecret = null;
        openNextSecret();
        item.resolve();
        return;
      }
      item.closing = true;
      closingSecret = item;
      if (dialog.open) dialog.close();
      else finalizeClose();
    };
    const closed = () => {
      if (item.closing) finalizeClose();
      else finish();
    };
    title.textContent = item.title;
    input.value = item.value;
    copy.onclick = async () => {
      const copied = await ui.copy(item.value);
      ui.toast(copied ? 'Copied to clipboard' : 'Copy failed; select the value manually', copied ? 'success' : 'warning');
    };
    dialog.querySelectorAll('[data-secret-close]').forEach(button => button.onclick = finish);
    dialog.addEventListener('close', closed, { once: true });
    if (!dialog.open) dialog.showModal();
    copy.focus();
  }

  ui.secret = function (title, value, returnFocus = document.activeElement) {
    return new Promise(resolve => {
      secretQueue.push({ title, value, returnFocus, returnFocusFallback: returnFocus?.closest?.('.table-wrap'), resolve });
      openNextSecret();
    });
  };

  ui.setConnection = function (state) {
    const labels = { connected: 'Connected', connecting: 'Connecting', reconnecting: 'Reconnecting', stale: 'Stale', signed_out: 'Signed out', unavailable: 'Unavailable' };
    document.querySelectorAll('[data-connection]').forEach(node => {
      node.textContent = labels[state] || state;
      node.closest('.connection-line')?.setAttribute('data-state', state);
    });
  };

  ui.setUpdated = function () {
    const now = new Date();
    document.querySelectorAll('[data-last-updated]').forEach(node => {
      node.textContent = now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
      node.dateTime = now.toISOString();
    });
  };

  ui.identityValue = function (identity, key) {
    return identity?.[key] ?? identity?.[key.toLowerCase()] ?? '';
  };

  ui.identityOptions = function (identities, key, selected = '') {
    const values = [...new Set((Array.isArray(identities) ? identities : [])
      .map(identity => ui.identityValue(identity, key)).filter(Boolean))];
    if (selected && !values.includes(selected)) values.push(selected);
    return values;
  };

  ui.fillIdentitySelect = function (select, identities, key, selected = select.value) {
    select.replaceChildren(new Option('All', ''));
    ui.identityOptions(identities, key, selected).forEach(value => select.append(new Option(value, value)));
    select.value = selected;
    return select;
  };

  ui.openDialog = function (dialog, returnFocus = document.activeElement) {
    dialog._returnFocus = returnFocus;
    dialog._returnFocusFallback = dialog._returnFocus?.closest?.('.table-wrap');
    dialog.showModal();
  };

  ui.stream = function (url, { event = 'change', onEvent, onOpen, onSafety } = {}) {
    let source;
    let staleTimer;
    let stopped = false;
    let lastSafety = 0;
    let disconnected = false;
    let stale = false;
    const admin = new URL(url, location.href).pathname.startsWith('/api/');
    const invoke = callback => {
      if (stopped || (admin && paused)) return;
      Promise.resolve().then(callback).catch(error => {
        if (!ui.isCanceled(error) && !paused) ui.setConnection('stale');
      });
    };
    const requestSafety = force => {
      if (document.hidden || (!force && Date.now() - lastSafety < 60000)) return;
      lastSafety = Date.now();
      invoke(() => onSafety?.());
    };
    const connect = () => {
      if (stopped || (admin && paused)) return;
      ui.setConnection('connecting');
      source = new EventSource(url);
      source.addEventListener('open', () => {
        if (stopped || (admin && paused)) return;
        disconnected = false;
        stale = false;
        clearTimeout(staleTimer);
        staleTimer = null;
        lastSafety = Date.now();
        ui.setConnection('connected');
        invoke(() => onOpen?.());
      });
      source.addEventListener(event, value => {
        try {
          const data = JSON.parse(value.data || '{}');
          invoke(() => onEvent?.(data));
        } catch (_) {
          ui.setConnection('stale');
          requestSafety();
        }
      });
      source.addEventListener('error', () => {
        if (stopped || (admin && paused)) return;
        disconnected = true;
        ui.setConnection(stale ? 'stale' : 'reconnecting');
        if (!staleTimer) staleTimer = setTimeout(() => {
          stale = true;
          if (!stopped && disconnected && !(admin && paused)) ui.setConnection('stale');
        }, 45000);
        requestSafety();
      });
    };
    const entry = {
      pause() { if (admin) { disconnected = false; stale = false; clearTimeout(staleTimer); staleTimer = null; source?.close(); } },
      resume() { if (admin) connect(); }
    };
    streams.add(entry);
    connect();
    const interval = setInterval(() => requestSafety(false), 60000);
    const onVisibility = () => requestSafety(true);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      stopped = true;
      streams.delete(entry);
      clearInterval(interval);
      clearTimeout(staleTimer);
      document.removeEventListener('visibilitychange', onVisibility);
      source?.close();
    };
  };

  function init() {
    applyTheme(theme(), false);
    document.querySelectorAll('[data-theme-toggle]').forEach(button => {
      button.onclick = () => applyTheme(theme() === 'dark' ? 'light' : 'dark');
    });
    const saveBar = document.querySelector('.save-bar');
    if (saveBar) {
      const placeToasts = () => {
        const bounds = saveBar.getBoundingClientRect();
        const bottom = bounds.top < innerHeight && bounds.bottom > 0 ? innerHeight - bounds.top + 12 : 16;
        document.documentElement.style.setProperty('--toast-bottom', `${bottom}px`);
      };
      new ResizeObserver(placeToasts).observe(saveBar);
      window.addEventListener('scroll', placeToasts, { passive: true });
      window.addEventListener('resize', placeToasts);
      placeToasts();
    }
    const resume = document.getElementById('session-resume');
    if (resume) resume.onclick = () => ui.withButton(resume, async () => {
      try {
        const session = await fetch('/api/sessions', { cache: 'no-store' });
        if (!session.ok) throw new Error(session.status === 401 ? 'Sign in first, then resume' : 'Session check failed');
        const response = await fetch('/', { cache: 'no-store' });
        if (!response.ok || new URL(response.url).pathname === '/login') throw new Error('Sign in first, then resume');
        const html = new DOMParser().parseFromString(await response.text(), 'text/html');
        csrf = JSON.parse(html.getElementById('cfg')?.textContent || '{}').csrf || '';
        if (!csrf) throw new Error('Session check failed');
        document.querySelectorAll('input[name="csrf"]').forEach(input => { input.value = csrf; });
        paused = false;
        document.getElementById('session-notice').hidden = true;
        resume.blur();
        resumeListeners.forEach(callback => Promise.resolve().then(callback).catch(error => {
          if (!ui.isCanceled(error)) ui.toast(error.message, 'error');
        }));
        streams.forEach(stream => stream.resume());
      } catch (error) { ui.toast(error.message, 'error'); }
    }, 'Checking...');
    document.querySelectorAll('dialog').forEach(dialog => {
      dialog.addEventListener('click', event => {
        if (event.target === dialog) dialog.close();
      });
	  dialog.addEventListener('close', () => {
	    if (!dialog.open && dialog._returnFocus) restoreSecretFocus(dialog._returnFocus, dialog._returnFocusFallback);
	  });
    });
	document.querySelectorAll('.table-wrap').forEach(region => {
	  region.tabIndex = 0;
	  region.setAttribute('aria-label', 'Scrollable data table');
	});
    try {
      const flash = JSON.parse(sessionStorage.getItem('mytracker-flash'));
      sessionStorage.removeItem('mytracker-flash');
      if (flash?.message) ui.toast(flash.message, flash.kind);
    } catch (_) {}
    icons();
  }

  window.mytrackerUI = ui;
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
