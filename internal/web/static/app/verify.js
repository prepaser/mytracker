(function () {
  const ui = window.mytrackerUI;
  function value(v) {
    if (v === true) return 'Valid';
    if (v === false) return 'Invalid';
    if (v == null || v === '') return 'Not available';
    return String(v);
  }

  function badge(text, stale) {
    const el = document.createElement('span');
    el.className = `badge status-${stale ? 'stale' : String(text || 'unknown').toLowerCase()}`;
    el.textContent = `${text || 'unknown'}${stale ? ' · stale' : ''}`;
    return el;
  }

  function block(title, status, stale, rows) {
    const section = document.createElement('section');
    section.className = 'proof-block';
    const head = document.createElement('div');
    head.className = 'proof-block-head';
    const heading = document.createElement('h3');
    heading.textContent = title;
    head.append(heading, badge(status, stale));
    const dl = document.createElement('dl');
    rows.filter(row => row[1] !== undefined && row[1] !== null && row[1] !== '').forEach(([label, raw]) => {
      const dt = document.createElement('dt');
      const dd = document.createElement('dd');
      dt.textContent = label;
      dd.textContent = value(raw);
      dl.append(dt, dd);
    });
    section.append(head, dl);
    return section;
  }

  function showVerify(data) {
    const out = document.getElementById('verify-out');
    out.replaceChildren();
    out.append(block('Overall', data.overall, data.tsa_stale || data.ots_stale, [
      ['Location', data.id], ['Payload digest', data.digest], ['Payload', data.payload_ok ? 'Verified' : 'Mismatch']
    ]));
    if (data.trust_policy) out.append(block('Trust policy', 'configured', false, [
      ['Engine', `v${data.trust_policy.engine_version}`],
	  ['TSA roots', data.trust_policy.tsa_roots_configured ? 'Configured' : 'System CA only'],
	  ['TSA policies', data.trust_policy.tsa_policy_configured ? 'Configured' : 'Not configured'],
      ['OTS confirmations', data.trust_policy.ots_min_confirmations],
	  ['OTS required sources', data.trust_policy.ots_required_sources],
      ['Revocation checks', 'OCSP/CRL not checked online']
    ]));
    out.append(block('TSA evidence', data.tsa?.result || 'missing', data.tsa_stale, data.tsa ? [
      ['Generated', data.tsa.tsa_gen_time ? new Date(data.tsa.tsa_gen_time * 1000).toLocaleString() : ''],
      ['Policy OID', data.tsa.tsa_policy_oid], ['Nonce', data.tsa.tsa_nonce_status],
      ['Policy', data.tsa.tsa_policy_status], ['Trust', data.tsa.tsa_trust_mode],
      ['Signer', data.tsa.tsa_signer_subject], ['Trust anchor', data.tsa.tsa_trust_anchor_fingerprint],
      ['Accuracy', data.tsa.tsa_accuracy_ms ? `${data.tsa.tsa_accuracy_ms} ms` : ''], ['Reason', data.tsa.error_code]
    ] : [['Reason', 'No stored TSA verification']]));
    out.append(block('Bitcoin evidence', data.ots?.result || 'missing', data.ots_stale, data.ots ? [
      ['Block height', data.ots.bitcoin_height], ['Block hash', data.ots.bitcoin_block_hash],
      ['Proof of work', data.ots.bitcoin_pow_valid], ['Confirmations', data.ots.bitcoin_confirmations],
      ['Provider agreement', data.ots.source_count ? `${data.ots.agreeing_sources}/${data.ots.source_count}` : ''],
      ['Reason', data.ots.error_code]
    ] : [['Reason', 'No stored OTS verification']]));
    out.append(block('Verification jobs', 'status', false, (data.jobs || []).map(job => [job.kind.toUpperCase(), `${job.state} (attempt ${job.attempts}/${job.max_attempts})`])));
  }

  ui.createVerificationDialog = function ({ csrf }) {
    const dialog = document.getElementById('verify-dialog');
    const state = document.getElementById('verify-state');
    const history = document.getElementById('verify-history');
    const historyState = document.getElementById('verify-history-state');
    const button = document.getElementById('verify-now');
    const queued = new Map();
    const controllers = new Map();
    let currentID;
    let hasEvidence = false;
    let lastHistoryKey = '';
    let submitting = false;
    let active = false;

    async function latest(key, path) {
      controllers.get(key)?.abort();
      const controller = new AbortController();
      controllers.set(key, controller);
      try {
        const result = await ui.request(path, { signal: controller.signal });
        controller.signal.throwIfAborted();
        return result;
      } finally {
        if (controllers.get(key) === controller) controllers.delete(key);
      }
    }

    function notice(node, message, retry) {
      node.hidden = false;
      node.replaceChildren(document.createTextNode(message));
      if (retry) {
        const action = document.createElement('button');
        action.type = 'button';
        action.className = 'icon-text-button small';
        action.innerHTML = '<i data-lucide="refresh-cw"></i><span>Retry</span>';
        action.onclick = retry;
        node.append(action);
        ui.icons(node);
      }
    }

    async function loadHistory(id) {
      try {
        const result = await latest('history', `/api/verify/${id}/history?limit=20`);
        if (currentID !== id || !dialog.open) return;
        historyState.hidden = true;
        history.replaceChildren();
        for (const run of result.data || []) {
          const row = document.createElement('div');
          row.className = 'history-row';
          const kind = document.createElement('strong');
          kind.textContent = run.kind?.toUpperCase() || '';
          const time = document.createElement('time');
          time.textContent = new Date(run.checked_at * 1000).toLocaleString();
          row.append(kind, badge(run.result), time);
          history.append(row);
        }
        if (!history.childElementCount) history.textContent = 'No verification history yet';
      } catch (error) {
        if (!ui.isCanceled(error) && currentID === id && dialog.open) {
          historyState.className = 'resource-state resource-stale';
          notice(historyState, 'History could not be refreshed.', () => loadHistory(id));
          if (history.textContent === 'Loading history') history.textContent = '';
        }
      }
    }

    function settle(id, data) {
      const wanted = queued.get(id);
      if (!wanted) return;
      const jobs = data.jobs || [];
      const completed = wanted.every(target => jobs.some(job => job.id === target.id && job.generation >= target.generation && ['done', 'failed'].includes(job.state)));
      if (!completed) return;
      queued.delete(id);
      const failed = wanted.some(target => jobs.some(job => job.id === target.id && job.state === 'failed'));
      ui.toast(failed ? 'Verification job failed' : `Verification complete: ${data.overall}`, failed || ['invalid', 'error', 'missing'].includes(data.overall) ? 'error' : data.overall === 'verified' && !data.tsa_stale && !data.ots_stale ? 'success' : 'warning');
    }

    async function load(id) {
      try {
        const response = await latest(`verify-${id}`, `/api/verify/${id}`);
        const data = response.data;
        settle(id, data);
        if (currentID !== id || !dialog.open) return;
        hasEvidence = true;
        state.hidden = true;
        showVerify(data);
        active = (data.jobs || []).some(job => ['queued', 'running', 'retry'].includes(job.state));
        button.disabled = submitting || active;
        const key = `${data.tsa?.id || 0}:${data.ots?.id || 0}`;
        if (key !== lastHistoryKey) { lastHistoryKey = key; loadHistory(id); }
      } catch (error) {
        if (ui.isCanceled(error) || currentID !== id || !dialog.open) return;
        state.className = `resource-state ${hasEvidence ? 'resource-stale' : 'resource-error'}`;
        notice(state, `${hasEvidence ? 'Evidence is stale.' : 'Evidence could not be loaded.'} ${error.message}`, () => load(id));
      }
    }

    function stop() {
      controllers.forEach(controller => controller.abort());
      controllers.clear();
    }

    async function refresh() {
      const ids = new Set(queued.keys());
      if (dialog.open && currentID) ids.add(currentID);
      await Promise.all([...ids].map(load));
    }

    async function open(id, returnFocus = document.activeElement) {
      if (ui.isPaused()) {
        const notice = document.getElementById('session-notice');
        if (notice) notice.hidden = false;
        if (dialog.open) dialog.close();
        document.getElementById('session-resume')?.focus();
        return;
      }
      stop();
      currentID = Number(id);
      dialog.dataset.id = currentID;
      hasEvidence = false;
      lastHistoryKey = '';
      active = false;
      document.getElementById('verify-out').replaceChildren();
      history.textContent = 'Loading history';
      historyState.hidden = true;
      button.disabled = true;
      state.className = 'resource-state';
      notice(state, 'Loading evidence');
      if (!dialog.open) ui.openDialog(dialog, returnFocus);
      await load(currentID);
    }

    document.getElementById('verify-close').onclick = () => dialog.close();
    dialog.addEventListener('close', stop);
    button.onclick = async () => {
      const id = currentID;
      submitting = true;
      try {
        await ui.withButton(button, async () => {
          const result = await ui.request(`/api/verify/${id}`, { method: 'POST', headers: { 'X-CSRF-Token': csrf } });
          queued.set(id, result.data.jobs || []);
          ui.toast('Verification queued');
          await load(id);
        }, 'Queuing...');
      } catch (error) { if (!ui.isCanceled(error)) ui.toast(error.message, 'error'); }
      finally { submitting = false; button.disabled = active; }
    };
    ui.onResume(refresh);
    return {
      open, refresh, stop,
      handleChange(change) {
        if (!(change.topics || []).includes('verification')) return;
        if (change.all || !change.location_ids?.length) refresh();
        else {
          const ids = new Set(change.location_ids.map(Number));
          if (dialog.open && ids.has(currentID)) load(currentID);
          queued.forEach((_, id) => { if (ids.has(id) && (!dialog.open || id !== currentID)) load(id); });
        }
      }
    };
  };
})();
