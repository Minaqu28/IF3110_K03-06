import { api, ApiError, setCSRF } from './api.js';
import { navigate, onRoute } from './router.js';

const main = document.querySelector('#main');
const nav = document.querySelector('#navigation');
const notice = document.querySelector('#notice');
let session = null;
let rendering = 0;

function node(tag, text = '', attributes = {}) {
  const element = document.createElement(tag);
  if (text !== '' && text !== null && text !== undefined) element.textContent = String(text);
  for (const [name, value] of Object.entries(attributes)) {
    if (value !== undefined && value !== null && value !== false) {
      element.setAttribute(name, value === true ? '' : String(value));
    }
  }
  return element;
}

function link(text, href, attrs = {}) {
  return node('a', text, { href, 'data-link': '', ...attrs });
}

function message(text) {
  notice.textContent = text;
}

function heading(text) {
  document.title = `${text} · DBMP`;
  main.replaceChildren(node('h1', text));
}

function field(form, label, name, type = 'text', { required = true, placeholder = '', value = '' } = {}) {
  const id = `field-${name}-${Math.random().toString(36).slice(2, 7)}`;
  const wrapper = node('div', '', { class: 'field' });
  if (type === 'checkbox') {
    const lbl = node('label', '', { for: id, class: 'checkbox-label' });
    const input = document.createElement('input');
    input.id = id;
    input.name = name;
    input.type = 'checkbox';
    input.checked = Boolean(value);
    lbl.append(input, document.createTextNode(` ${label}`));
    wrapper.append(lbl);
    form.append(wrapper);
    return input;
  }
  wrapper.append(node('label', label, { for: id }));
  const input = type === 'textarea' ? document.createElement('textarea') : document.createElement('input');
  input.id = id;
  input.name = name;
  if (type !== 'textarea') input.type = type;
  else input.rows = 4;
  input.required = required;
  if (placeholder) input.placeholder = placeholder;
  if (value !== '' && value !== undefined && value !== null) input.value = String(value);
  if (name === 'username') input.autocomplete = 'username';
  if (name === 'password') input.autocomplete = 'current-password';
  wrapper.append(input);
  form.append(wrapper);
  return input;
}

function selectField(form, label, name, options, { required = true } = {}) {
  const id = `field-${name}-${Math.random().toString(36).slice(2, 7)}`;
  const wrapper = node('div', '', { class: 'field' });
  wrapper.append(node('label', label, { for: id }));
  const select = node('select', '', { id, name, required });
  for (const [value, text] of options) select.append(node('option', text, { value }));
  wrapper.append(select);
  form.append(wrapper);
  return select;
}

function renderTable(captionText, columns, rows) {
  const scroll = node('div', '', { class: 'table-scroll', role: 'region', 'aria-label': captionText, tabindex: '0' });
  const table = document.createElement('table');
  if (captionText) table.append(node('caption', captionText));
  const head = document.createElement('thead');
  const hr = document.createElement('tr');
  for (const col of columns || []) hr.append(node('th', col, { scope: 'col' }));
  head.append(hr);
  table.append(head);
  const body = document.createElement('tbody');
  for (const values of rows || []) {
    const tr = document.createElement('tr');
    for (const cell of values) {
      if (cell && typeof cell === 'object' && 'null' in cell) {
        tr.append(node('td', cell.null ? 'NULL' : cell.value));
      } else {
        tr.append(node('td', cell ?? ''));
      }
    }
    body.append(tr);
  }
  table.append(body);
  scroll.append(table);
  return scroll;
}

function adminForm(title, action, fields) {
  const form = document.createElement('form');
  form.append(node('h2', title));
  const inputs = {};
  for (const spec of fields) {
    const [label, name, type = 'text', options = null, extra = {}] = spec;
    if (options) {
      inputs[name] = selectField(form, label, name, options, extra);
    } else {
      const input = field(form, label, name, type, extra);
      if (type === 'number' && !extra.allowZero) input.min = '1';
      inputs[name] = input;
    }
  }
  const error = node('p', '', { role: 'alert', class: 'error' });
  const submit = node('button', title, { type: 'submit' });
  form.append(error, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    submit.disabled = true;
    error.textContent = '';
    const body = { action };
    for (const [name, input] of Object.entries(inputs)) {
      if (input.type === 'checkbox') {
        body[name] = input.checked;
      } else if (input.type === 'number') {
        if (input.value !== '') body[name] = Number(input.value);
      } else if (input.value !== '' || input.required) {
        body[name] = input.value;
      }
    }
    try {
      await api('/admin/actions', { method: 'POST', body });
      session = await api('/auth/session');
      setCSRF(session.csrf_token);
      navigate('/admin', true);
      message(`${title} berhasil.`);
    } catch (failure) {
      error.textContent = failure.message;
    } finally {
      submit.disabled = false;
    }
  });
  return form;
}

function renderNavigation() {
  nav.replaceChildren();
  if (!session) return;
  nav.append(link('Dashboard', '/dashboard'), link('Database', '/databases'));
  if (session.user.is_platform_admin) nav.append(link('Admin', '/admin'));
  const logout = node('button', 'Keluar', { type: 'button' });
  logout.addEventListener('click', async () => {
    try {
      await api('/auth/logout', { method: 'POST' });
      session = null;
      setCSRF('');
      navigate('/login');
    } catch (error) {
      message(error.message);
    }
  });
  nav.append(logout);
}

function renderLogin() {
  heading('Masuk');
  const form = document.createElement('form');
  const username = field(form, 'Username', 'username');
  const password = field(form, 'Password', 'password', 'password');
  const errorBox = node('p', '', { role: 'alert', class: 'error' });
  const submit = node('button', 'Masuk', { type: 'submit' });
  form.append(errorBox, submit);
  main.append(form);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    submit.disabled = true;
    errorBox.textContent = '';
    try {
      const { csrf_token } = await api('/auth/login-csrf');
      await api('/auth/login', { method: 'POST', csrf: csrf_token, body: { username: username.value, password: password.value } });
      session = await api('/auth/session');
      setCSRF(session.csrf_token);
      navigate('/dashboard');
    } catch (error) {
      errorBox.textContent = error.message;
    } finally {
      submit.disabled = false;
    }
  });
  username.focus();
}

function renderDashboard() {
  heading('Dashboard');
  const panel = node('section', '', { class: 'panel' });
  panel.append(
    node('p', `Halo, ${session.user.username}.`),
    node('p', session.user.is_platform_admin ? 'Peran: Platform Admin' : 'Peran: Pengguna'),
  );
  const actions = node('div', '', { class: 'action-row' });
  actions.append(link('Lihat database yang dapat diakses', '/databases', { class: 'button-link' }));
  if (session.user.is_platform_admin) {
    actions.append(link('Administrasi Platform', '/admin', { class: 'button-link' }));
  }
  panel.append(actions);
  main.append(panel);
}

async function renderDatabases(ticket) {
  heading('Database');
  main.append(node('p', 'Memuat database...'));
  const databases = await api('/databases');
  if (ticket !== rendering) return;
  heading('Database');
  const list = node('ul', '', { class: 'card-list' });
  for (const database of databases || []) {
    const item = document.createElement('li');
    item.append(link(`#${database.id} · ${database.name}`, `/databases/${database.id}`));
    list.append(item);
  }
  main.append(databases?.length ? list : node('p', 'Belum ada database yang dapat diakses.'));
}

function databaseSubNav(databaseID, active = '') {
  const sub = node('nav', '', { class: 'sub-nav', 'aria-label': 'Fitur database' });
  const items = [
    ['Explorer & SQL', `/databases/${databaseID}`, 'explorer'],
    ['Revisi Migration', `/databases/${databaseID}/revisions`, 'revisions'],
    ['Review & Approval', `/databases/${databaseID}/reviews`, 'reviews'],
    ['Riwayat Eksekusi', `/databases/${databaseID}/history`, 'history'],
  ];
  for (const [label, href, key] of items) {
    sub.append(link(label, href, { class: key === active ? 'active' : '' }));
  }
  return sub;
}

async function renderDatabase(idWithSearch, ticket) {
  const databaseID = idWithSearch.split('?')[0];
  heading('Database');
  main.append(node('p', 'Memuat tabel...'));
  const view = await api(`/databases/${idWithSearch}`);
  if (ticket !== rendering) return;
  heading(`${view.connection.name} (${view.level})`);
  main.append(databaseSubNav(databaseID, 'explorer'));
  if (view.error) main.append(node('p', view.error, { role: 'alert', class: 'error' }));

  // Schema selector
  if (view.schemas?.length) {
    const schemaPanel = node('section', '', { class: 'panel' });
    const row = node('div', '', { class: 'action-row' });
    row.append(node('strong', 'Schema:'));
    for (const s of view.schemas) {
      row.append(link(s, `/databases/${databaseID}?schema=${encodeURIComponent(s)}`, { class: s === view.schema ? 'active-pill' : 'pill' }));
    }
    schemaPanel.append(row);
    main.append(schemaPanel);
  }

  // Tables
  const tablePanel = node('section', '', { class: 'panel' });
  tablePanel.append(node('h2', `Tabel pada schema "${view.schema}"`));
  const list = node('ul', '', { class: 'card-list' });
  for (const table of view.tables || []) {
    const item = document.createElement('li');
    const target = `/databases/${databaseID}?schema=${encodeURIComponent(table.schema)}&table=${encodeURIComponent(table.name)}`;
    const countLabel = table.rows >= 0 ? ` (${table.rows} rows)` : '';
    item.append(link(`${table.schema}.${table.name}${countLabel}`, target));
    list.append(item);
  }
  tablePanel.append(view.tables?.length ? list : node('p', 'Tidak ada tabel pada schema ini.'));
  main.append(tablePanel);

  // Table details + preview
  if (view.details) {
    const detailsPanel = node('section', '', { class: 'panel' });
    detailsPanel.append(node('h2', `Struktur ${view.schema}.${view.table}`));
    detailsPanel.append(renderTable(
      'Kolom',
      ['Nama', 'Tipe', 'Nullable', 'Default'],
      (view.details.columns || []).map(c => [c.name, c.type, c.nullable ? 'YES' : 'NO', c.default || '-']),
    ));
    if (view.details.constraints?.length) {
      detailsPanel.append(renderTable(
        'Constraint',
        ['Nama', 'Tipe', 'Definisi'],
        view.details.constraints.map(c => [c.name, c.type, c.definition]),
      ));
    }
    if (view.details.indexes?.length) {
      detailsPanel.append(renderTable(
        'Index',
        ['Nama', 'Definisi'],
        view.details.indexes.map(i => [i.name, i.definition]),
      ));
    }
    main.append(detailsPanel);
  }

  if (view.preview) {
    const previewPanel = node('section', '', { class: 'panel' });
    previewPanel.append(renderTable(`Data ${view.table} (halaman ${view.page})`, view.preview.columns, view.preview.rows));
    const pager = node('div', '', { class: 'action-row' });
    const base = `/databases/${databaseID}?schema=${encodeURIComponent(view.schema)}&table=${encodeURIComponent(view.table)}`;
    if (view.has_prev) pager.append(link('← Halaman sebelumnya', `${base}&page=${view.page - 1}`, { class: 'button-link' }));
    if (view.has_next) pager.append(link('Halaman berikutnya →', `${base}&page=${view.page + 1}`, { class: 'button-link' }));
    if (pager.childElementCount) previewPanel.append(pager);
    main.append(previewPanel);
  }

  // SQL Editor
  const sqlForm = document.createElement('form');
  sqlForm.append(node('h2', view.can_edit ? 'SQL Editor' : 'SQL Query (Read-Only)'));
  const sqlInput = field(sqlForm, 'SQL', 'sql', 'textarea', { placeholder: 'SELECT * FROM ... LIMIT 20;' });
  const sqlError = node('p', '', { role: 'alert', class: 'error' });
  const sqlSubmit = node('button', 'Jalankan SQL', { type: 'submit' });
  const sqlResults = node('div');
  sqlForm.append(sqlError, sqlSubmit, sqlResults);
  sqlForm.addEventListener('submit', async event => {
    event.preventDefault();
    sqlSubmit.disabled = true;
    sqlError.textContent = '';
    sqlResults.replaceChildren(node('p', 'Menjalankan query...'));
    try {
      const data = await api(`/databases/${databaseID}/query`, { method: 'POST', body: { sql: sqlInput.value } });
      sqlResults.replaceChildren();
      if (data.error) sqlError.textContent = data.error;
      for (const res of data.results || []) {
        if (res.error) {
          sqlResults.append(node('p', res.error, { role: 'alert', class: 'error' }));
        } else if (res.columns?.length) {
          sqlResults.append(renderTable(res.command || 'Hasil', res.columns, res.rows));
        } else {
          sqlResults.append(node('p', res.command || 'Berhasil.'));
        }
      }
    } catch (failure) {
      sqlResults.replaceChildren();
      sqlError.textContent = failure.message;
    } finally {
      sqlSubmit.disabled = false;
    }
  });
  main.append(sqlForm);

  // EXPLAIN Analyzer
  const planForm = document.createElement('form');
  planForm.append(node('h2', 'Analisis Execution Plan (EXPLAIN)'));
  const planInput = field(planForm, 'Query SELECT', 'plan_sql', 'textarea', { placeholder: 'SELECT * FROM ...' });
  const planMode = selectField(planForm, 'Mode', 'mode', [['estimate', 'EXPLAIN (Estimate)'], ['analyze', 'EXPLAIN ANALYZE']]);
  const planError = node('p', '', { role: 'alert', class: 'error' });
  const planSubmit = node('button', 'Analisis Plan', { type: 'submit' });
  const planOut = node('div');
  planForm.append(planError, planSubmit, planOut);
  planForm.addEventListener('submit', async event => {
    event.preventDefault();
    planSubmit.disabled = true;
    planError.textContent = '';
    planOut.replaceChildren(node('p', 'Menganalisis execution plan...'));
    try {
      const data = await api(`/databases/${databaseID}/explain`, { method: 'POST', body: { sql: planInput.value, mode: planMode.value } });
      planOut.replaceChildren();
      if (data.error) planError.textContent = data.error;
      if (data.plan) {
        const pre = node('pre', JSON.stringify(data.plan, null, 2), { class: 'sql-block' });
        planOut.append(pre);
      }
    } catch (failure) {
      planOut.replaceChildren();
      planError.textContent = failure.message;
    } finally {
      planSubmit.disabled = false;
    }
  });
  main.append(planForm);

  // Recent Audit Change Log
  if (view.changes?.length) {
    const changePanel = node('section', '', { class: 'panel' });
    changePanel.append(node('h2', 'Log Perubahan Terbaru'));
    changePanel.append(renderTable(
      'Change Log',
      ['Waktu', 'Sumber', 'User', 'Status', 'Durasi (ms)', 'SQL'],
      view.changes.map(c => [c.created_at, c.source, c.username || '-', c.status, c.duration_ms, c.sql]),
    ));
    main.append(changePanel);
  }
}

async function renderRevisions(databaseID, ticket) {
  heading('Revisi Migration');
  main.append(node('p', 'Memuat revisi...'));
  const [revisions, reviews] = await Promise.all([
    api(`/databases/${databaseID}/revisions`),
    api(`/databases/${databaseID}/reviews`).catch(() => []),
  ]);
  if (ticket !== rendering) return;
  heading('Revisi Migration');
  main.append(databaseSubNav(databaseID, 'revisions'));

  const approvedApplyByRevision = new Map();
  const approvedForwardByRevision = new Map();
  for (const r of reviews || []) {
    if (r.status !== 'APPROVED') continue;
    const act = r.action || 'APPLY';
    if (act === 'APPLY' && !approvedApplyByRevision.has(r.revision_id)) {
      approvedApplyByRevision.set(r.revision_id, r);
    }
    if (act === 'FORWARD' && !approvedForwardByRevision.has(r.revision_id)) {
      approvedForwardByRevision.set(r.revision_id, r);
    }
  }

  // Create revision form
  const form = document.createElement('form');
  form.append(node('h2', 'Buat Revisi Migration Baru'));
  const migIdInput = field(form, 'Migration ID (kosongkan untuk migration baru)', 'migration_id', 'number', { required: false });
  const titleInput = field(form, 'Judul Migration', 'title', 'text', { placeholder: 'Tambah kolom status pada orders' });
  const upInput = field(form, 'SQL Up', 'up_sql', 'textarea', { placeholder: 'ALTER TABLE ... atau UPDATE ... WHERE id > $1 AND id <= $2' });
  const reversibleInput = field(form, 'Reversible (memiliki SQL Down)', 'reversible', 'checkbox', { required: false, value: true });
  const downInput = field(form, 'SQL Down (wajib jika reversible)', 'down_sql', 'textarea', { required: false, placeholder: 'ALTER TABLE ...' });
  const reasonInput = field(form, 'Alasan Irreversible (wajib jika tidak reversible)', 'irreversible_reason', 'text', { required: false });
  const batchToggle = field(form, 'Mode Batch (untuk data besar)', 'is_batch', 'checkbox', { required: false, value: false });
  const batchTable = field(form, 'Batch Table (mis. public.orders)', 'batch_table', 'text', { required: false });
  const batchCursor = field(form, 'Batch Cursor Column (mis. id)', 'batch_cursor_column', 'text', { required: false });
  const batchSize = field(form, 'Batch Size (1 - 100000)', 'batch_size', 'number', { required: false, value: '1000' });
  const formErr = node('p', '', { role: 'alert', class: 'error' });
  const submitBtn = node('button', 'Simpan Draft Revisi', { type: 'submit' });
  form.append(formErr, submitBtn);

  form.addEventListener('submit', async event => {
    event.preventDefault();
    submitBtn.disabled = true;
    formErr.textContent = '';
    const body = {
      title: titleInput.value,
      up_sql: upInput.value,
      reversible: reversibleInput.checked,
      down_sql: reversibleInput.checked ? downInput.value : '',
      irreversible_reason: reversibleInput.checked ? '' : reasonInput.value,
    };
    if (migIdInput.value) body.migration_id = Number(migIdInput.value);
    if (batchToggle.checked) {
      body.batch = {
        table: batchTable.value,
        cursor_column: batchCursor.value,
        batch_size: Number(batchSize.value),
      };
    }
    try {
      await api(`/databases/${databaseID}/revisions`, { method: 'POST', body });
      message('Draft revisi berhasil dibuat.');
      await renderRevisions(databaseID, ticket);
    } catch (failure) {
      formErr.textContent = failure.message;
    } finally {
      submitBtn.disabled = false;
    }
  });
  main.append(form);

  // List revisions
  const list = node('ul', '', { class: 'stack-list' });
  for (const rev of revisions || []) {
    const li = node('li', '', { class: 'panel' });
    const mode = rev.batch ? 'BATCH' : 'NORMAL';
    li.append(
      node('strong', `Migration #${rev.migration_id} · Revisi #${rev.revision} (ID ${rev.id}) — ${rev.title || '(tanpa judul)'}`),
      node('p', `Status: ${rev.status} · Mode: ${mode} · ${rev.reversible ? 'Reversible' : `Irreversible (${rev.irreversible_reason})`}`),
    );
    if (rev.batch) {
      li.append(node('p', `Batch config: table=${rev.batch.table}, cursor=${rev.batch.cursor_column}, size=${rev.batch.batch_size}`));
    }
    li.append(node('p', 'SQL Up:'), node('pre', rev.up_sql, { class: 'sql-block' }));
    if (rev.down_sql) li.append(node('p', 'SQL Down:'), node('pre', rev.down_sql, { class: 'sql-block' }));

    const actions = node('div', '', { class: 'action-row' });
    if (rev.status === 'DRAFT') {
      const reviewBtn = node('button', 'Ajukan Review (APPLY)', { type: 'button' });
      reviewBtn.addEventListener('click', async () => {
        reviewBtn.disabled = true;
        try {
          await api(`/databases/${databaseID}/revisions/${rev.id}/review`, { method: 'POST', body: { action: 'APPLY' } });
          message(`Revisi #${rev.id} diajukan untuk review APPLY.`);
          await renderRevisions(databaseID, ticket);
        } catch (failure) {
          message(failure.message);
          reviewBtn.disabled = false;
        }
      });
      actions.append(reviewBtn);
    }

    const approvedApply = approvedApplyByRevision.get(rev.id);
    if (rev.status === 'APPROVED' && approvedApply) {
      const runBtn = node('button', 'Jalankan (APPLY)', { type: 'button' });
      runBtn.addEventListener('click', async () => {
        runBtn.disabled = true;
        try {
          const job = await api(`/databases/${databaseID}/executions`, {
            method: 'POST',
            body: {
              migration_id: rev.migration_id,
              revision_id: rev.id,
              approval_request_id: approvedApply.id,
              action: 'APPLY',
              mode,
            },
          });
          message(`Job APPLY ${job.id} dimasukkan ke queue.`);
          navigate(`/jobs/${encodeURIComponent(job.id)}`);
        } catch (failure) {
          message(failure.message);
          runBtn.disabled = false;
        }
      });
      actions.append(runBtn);
    }

    if (rev.status === 'APPROVED') {
      const approvedForward = approvedForwardByRevision.get(rev.id);
      if (approvedForward) {
        const fwdBtn = node('button', 'Jalankan (FORWARD)', { type: 'button' });
        fwdBtn.addEventListener('click', async () => {
          fwdBtn.disabled = true;
          try {
            const job = await api(`/databases/${databaseID}/executions`, {
              method: 'POST',
              body: {
                migration_id: rev.migration_id,
                revision_id: rev.id,
                approval_request_id: approvedForward.id,
                action: 'FORWARD',
                mode,
              },
            });
            message(`Job FORWARD ${job.id} dimasukkan ke queue.`);
            navigate(`/jobs/${encodeURIComponent(job.id)}`);
          } catch (failure) {
            message(failure.message);
            fwdBtn.disabled = false;
          }
        });
        actions.append(fwdBtn);
      }
      const reqFwdBtn = node('button', 'Ajukan Review Forward', { type: 'button' });
      reqFwdBtn.addEventListener('click', async () => {
        reqFwdBtn.disabled = true;
        try {
          await api(`/databases/${databaseID}/revisions/${rev.id}/review`, { method: 'POST', body: { action: 'FORWARD' } });
          message(`Permintaan review FORWARD untuk revisi #${rev.id} dibuat.`);
          navigate(`/databases/${databaseID}/reviews`);
        } catch (failure) {
          message(failure.message);
          reqFwdBtn.disabled = false;
        }
      });
      actions.append(reqFwdBtn);
    }
    if (actions.childElementCount) li.append(actions);
    list.append(li);
  }
  main.append(revisions?.length ? list : node('p', 'Belum ada revisi migration.'));
}

async function renderReviews(databaseID, ticket) {
  heading('Review & Approval');
  main.append(node('p', 'Memuat review...'));
  const [reviews, revisions] = await Promise.all([
    api(`/databases/${databaseID}/reviews`),
    api(`/databases/${databaseID}/revisions`).catch(() => []),
  ]);
  if (ticket !== rendering) return;
  heading('Review & Approval');
  main.append(databaseSubNav(databaseID, 'reviews'));

  if (!reviews?.length) {
    main.append(node('p', 'Belum ada permintaan review.'));
    return;
  }

  const revisionById = new Map((revisions || []).map(r => [r.id, r]));
  const list = node('ul', '', { class: 'stack-list' });
  for (const rev of reviews) {
    const li = node('li', '', { class: 'panel' });
    const act = rev.action || 'APPLY';
    li.append(
      node('strong', `Review #${rev.id} · Revisi #${rev.revision_id} · Aksi: ${act}`),
      node('p', `Status: ${rev.status} · Hash: ${rev.definition_hash?.slice(0, 12) || '-'}`),
    );
    if (rev.target_execution_id) {
      li.append(node('p', `Target Execution ID: ${rev.target_execution_id}`));
    }
    const decisionsBox = node('div');
    decisionsBox.append(node('p', 'Memuat keputusan leader...'));
    li.append(decisionsBox);

    api(`/databases/${databaseID}/reviews/${rev.id}/decisions`)
      .then(decisions => {
        if (ticket !== rendering) return;
        decisionsBox.replaceChildren();
        if (decisions?.length) {
          const ul = document.createElement('ul');
          for (const d of decisions) {
            ul.append(node('li', `Group #${d.group_id} (Leader #${d.leader_id}): ${d.status}${d.comment ? ` — "${d.comment}"` : ''}`));
          }
          decisionsBox.append(ul);
        }
      })
      .catch(() => { decisionsBox.replaceChildren(); });

    if (rev.status === 'PENDING') {
      const form = document.createElement('form');
      form.className = 'inline-form';
      const commentInput = field(form, 'Catatan (opsional)', `comment-${rev.id}`, 'text', { required: false });
      const errBox = node('p', '', { role: 'alert', class: 'error' });
      const actions = node('div', '', { class: 'action-row' });
      const decide = async status => {
        errBox.textContent = '';
        try {
          await api(`/reviews/${rev.id}/decision`, { method: 'POST', body: { status, comment: commentInput.value } });
          message(`Keputusan ${status} tersimpan.`);
          await renderReviews(databaseID, ticket);
        } catch (failure) {
          errBox.textContent = failure.message;
        }
      };
      const approveBtn = node('button', 'Approve', { type: 'button' });
      approveBtn.addEventListener('click', () => decide('APPROVED'));
      const rejectBtn = node('button', 'Reject', { type: 'button' });
      rejectBtn.addEventListener('click', () => decide('REJECTED'));
      actions.append(approveBtn, rejectBtn);
      form.append(errBox, actions);
      li.append(form);
    } else if (rev.status === 'APPROVED') {
      const targetRev = revisionById.get(rev.revision_id);
      if (targetRev) {
        const execBtn = node('button', `Queue Eksekusi (${act})`, { type: 'button' });
        execBtn.addEventListener('click', async () => {
          execBtn.disabled = true;
          try {
            const payload = {
              migration_id: targetRev.migration_id,
              revision_id: targetRev.id,
              approval_request_id: rev.id,
              action: act,
              mode: targetRev.batch ? 'BATCH' : 'NORMAL',
            };
            if (rev.target_execution_id) payload.target_execution_id = rev.target_execution_id;
            const job = await api(`/databases/${databaseID}/executions`, { method: 'POST', body: payload });
            message(`Job ${act} ${job.id} dimasukkan ke queue.`);
            navigate(`/jobs/${encodeURIComponent(job.id)}`);
          } catch (failure) {
            message(failure.message);
            execBtn.disabled = false;
          }
        });
        li.append(execBtn);
      }
    }
    list.append(li);
  }
  main.append(list);
}

async function renderHistory(databaseID, ticket) {
  const params = new URLSearchParams(location.search);
  const hideRollback = params.get('hide_rollback') === 'true' || params.get('hide_rollback') === '1';
  heading('Riwayat Eksekusi');
  main.append(node('p', 'Memuat riwayat...'));
  const [items, revisions, reviews] = await Promise.all([
    api(`/databases/${databaseID}/history${hideRollback ? '?hide_rollback=true' : ''}`),
    api(`/databases/${databaseID}/revisions`).catch(() => []),
    api(`/databases/${databaseID}/reviews`).catch(() => []),
  ]);
  if (ticket !== rendering) return;
  heading('Riwayat Eksekusi');
  main.append(databaseSubNav(databaseID, 'history'));

  // Hide rollback checkbox (SPECS.txt required)
  const filterPanel = node('section', '', { class: 'panel' });
  const label = node('label', '', { class: 'checkbox-label' });
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = hideRollback;
  cb.addEventListener('change', () => {
    const target = `/databases/${databaseID}/history${cb.checked ? '?hide_rollback=true' : ''}`;
    navigate(target, true);
  });
  label.append(cb, document.createTextNode(' Hide rollback (sembunyikan pasangan eksekusi yang dibatalkan & rollback sukses)'));
  filterPanel.append(label);
  main.append(filterPanel);

  const revisionById = new Map((revisions || []).map(r => [r.id, r]));
  const approvedRollbackByTarget = new Map();
  for (const r of reviews || []) {
    if (r.status === 'APPROVED' && r.action === 'ROLLBACK' && r.target_execution_id && !approvedRollbackByTarget.has(r.target_execution_id)) {
      approvedRollbackByTarget.set(r.target_execution_id, r);
    }
  }

  // History list
  const list = node('ul', '', { class: 'stack-list' });
  for (const item of items || []) {
    const li = node('li', '', { class: 'panel' });
    const meta = [];
    if (item.migration_id) meta.push(`Migration #${item.migration_id}${item.migration_version ? ` (v${item.migration_version})` : ''}`);
    if (item.revision) meta.push(`Revisi #${item.revision}`);
    if (item.action) meta.push(`${item.action}${item.mode ? ` (${item.mode})` : ''}`);
    const actorLabel = item.actor_username || (item.actor_id ? `User #${item.actor_id}` : '-');
    li.append(
      node('strong', `Attempt #${item.attempt_id} (percobaan ke-${item.attempt_number}) — ${item.status}`),
      meta.length ? node('p', meta.join(' · ')) : document.createTextNode(''),
      node('p', `Execution ID: ${item.execution_id} · Pengguna: ${actorLabel}`),
      item.reverts_execution_id ? node('p', `Membatalkan Execution ID: ${item.reverts_execution_id}`) : document.createTextNode(''),
      node('p', `Mulai: ${item.started_at}${item.finished_at ? ` · Selesai: ${item.finished_at}` : ''}`),
    );
    if (item.error_code || item.error_message) {
      li.append(node('p', `Error [${item.error_code || '-'}]: ${item.error_message}`, { role: 'alert', class: 'error' }));
    }
    li.append(node('pre', item.sql_snapshot, { class: 'sql-block' }));

    const rowActions = node('div', '', { class: 'action-row' });
    const rev = revisionById.get(item.revision_id);
    if (item.action !== 'ROLLBACK' && rev && rev.reversible && (item.status === 'SUCCEEDED' || item.status === 'PARTIAL')) {
      const approvedRb = approvedRollbackByTarget.get(item.execution_id);
      if (approvedRb) {
        const execRbBtn = node('button', 'Queue Rollback (Approved)', { type: 'button' });
        execRbBtn.addEventListener('click', async () => {
          execRbBtn.disabled = true;
          try {
            const job = await api(`/databases/${databaseID}/executions`, {
              method: 'POST',
              body: {
                migration_id: rev.migration_id,
                revision_id: rev.id,
                approval_request_id: approvedRb.id,
                action: 'ROLLBACK',
                mode: rev.batch ? 'BATCH' : 'NORMAL',
                target_execution_id: item.execution_id,
              },
            });
            message(`Rollback job ${job.id} dimasukkan ke queue.`);
            navigate(`/jobs/${encodeURIComponent(job.id)}`);
          } catch (failure) {
            message(failure.message);
            execRbBtn.disabled = false;
          }
        });
        rowActions.append(execRbBtn);
      } else {
        const reqRbBtn = node('button', 'Ajukan Review Rollback', { type: 'button' });
        reqRbBtn.addEventListener('click', async () => {
          reqRbBtn.disabled = true;
          try {
            await api(`/databases/${databaseID}/revisions/${rev.id}/review`, {
              method: 'POST',
              body: { action: 'ROLLBACK', target_execution_id: item.execution_id },
            });
            message(`Review ROLLBACK untuk execution ${item.execution_id} diajukan.`);
            navigate(`/databases/${databaseID}/reviews`);
          } catch (failure) {
            message(failure.message);
            reqRbBtn.disabled = false;
          }
        });
        rowActions.append(reqRbBtn);
      }
    }
    if (rowActions.childElementCount) li.append(rowActions);
    list.append(li);
  }
  main.append(items?.length ? list : node('p', 'Belum ada riwayat eksekusi.'));
}

async function renderJob(id, ticket) {
  heading('Status Job');
  main.append(node('p', 'Memuat status...'));
  const job = await api(`/jobs/${encodeURIComponent(id)}`);
  if (ticket !== rendering) return;
  heading('Status Job');
  const panel = node('section', '', { class: 'panel' });
  panel.append(
    node('p', `Job ID: ${job.id}`),
    node('p', `Execution ID: ${job.execution_id}`),
    node('p', `Database ID: ${job.database_id}`),
    node('p', `Status: ${job.status} (Attempt: ${job.attempt_count})`),
  );
  if (job.action || job.mode || job.migration_id) {
    panel.append(node('p', `Migration #${job.migration_id || '-'} · Revisi #${job.revision_id || '-'} · Aksi: ${job.action || '-'} (${job.mode || '-'})`));
  }
  if (job.target_execution_id) {
    panel.append(node('p', `Target Execution ID: ${job.target_execution_id}`));
  }
  if (job.progress) {
    const total = job.progress.total == null ? '' : ` dari ${job.progress.total}`;
    panel.append(
      node('p', `Batch selesai: ${job.progress.completed}${total} (${job.progress.status})`),
      node('p', `Affected rows: ${job.progress.affected_rows}${job.progress.last_cursor ? ` · Last cursor: ${job.progress.last_cursor}` : ''}`),
    );
  }
  if (job.error_code || job.error_message) {
    panel.append(node('p', `Error [${job.error_code || '-'}]: ${job.error_message}`, { role: 'alert', class: 'error' }));
  }

  const actionRow = node('div', '', { class: 'action-row' });
  if (job.can_resume && job.migration_id && job.revision_id && job.approval_request_id) {
    const resumeBtn = node('button', 'Lanjutkan dari Checkpoint (Resume)', { type: 'button' });
    resumeBtn.addEventListener('click', async () => {
      resumeBtn.disabled = true;
      try {
        const body = {
          execution_id: job.execution_id,
          migration_id: job.migration_id,
          revision_id: job.revision_id,
          approval_request_id: job.approval_request_id,
          action: job.action,
          mode: job.mode,
        };
        if (job.target_execution_id) body.target_execution_id = job.target_execution_id;
        const nextJob = await api(`/databases/${job.database_id}/executions`, { method: 'POST', body });
        message(`Resume job ${nextJob.id} dimasukkan ke queue.`);
        navigate(`/jobs/${encodeURIComponent(nextJob.id)}`);
      } catch (failure) {
        message(failure.message);
        resumeBtn.disabled = false;
      }
    });
    actionRow.append(resumeBtn);
  }
  if (job.can_rollback && job.revision_id) {
    const rbBtn = node('button', 'Ajukan Review Rollback', { type: 'button' });
    rbBtn.addEventListener('click', async () => {
      rbBtn.disabled = true;
      try {
        await api(`/databases/${job.database_id}/revisions/${job.revision_id}/review`, {
          method: 'POST',
          body: { action: 'ROLLBACK', target_execution_id: job.execution_id },
        });
        message(`Review ROLLBACK untuk execution ${job.execution_id} diajukan.`);
        navigate(`/databases/${job.database_id}/reviews`);
      } catch (failure) {
        message(failure.message);
        rbBtn.disabled = false;
      }
    });
    actionRow.append(rbBtn);
  }
  if (job.can_forward && job.revision_id) {
    const fwdBtn = node('button', 'Ajukan Review Forward', { type: 'button' });
    fwdBtn.addEventListener('click', async () => {
      fwdBtn.disabled = true;
      try {
        await api(`/databases/${job.database_id}/revisions/${job.revision_id}/review`, {
          method: 'POST',
          body: { action: 'FORWARD' },
        });
        message(`Review FORWARD untuk revisi #${job.revision_id} diajukan.`);
        navigate(`/databases/${job.database_id}/reviews`);
      } catch (failure) {
        message(failure.message);
        fwdBtn.disabled = false;
      }
    });
    actionRow.append(fwdBtn);
  }
  if (actionRow.childElementCount) panel.append(actionRow);

  const navRow = node('div', '', { class: 'action-row' });
  navRow.append(
    link('Ke Riwayat Database', `/databases/${job.database_id}/history`, { class: 'button-link' }),
    link('Ke Review Database', `/databases/${job.database_id}/reviews`, { class: 'button-link' }),
    link('Ke Revisi Database', `/databases/${job.database_id}/revisions`, { class: 'button-link' }),
  );
  panel.append(navRow);
  main.append(panel);

  if (['QUEUED', 'RUNNING', 'RETRY_WAIT', 'NEEDS_RECONCILIATION'].includes(job.status)) {
    main.append(node('p', 'Status akan diperbarui otomatis setiap 2 detik...'));
    setTimeout(() => {
      if (ticket === rendering) renderJob(id, ticket).catch(error => message(error.message));
    }, 2000);
  }
}

async function renderAdmin(ticket) {
  if (!session.user.is_platform_admin) throw new ApiError(403, 'FORBIDDEN', 'Akses admin ditolak.');
  heading('Admin Platform');
  main.append(node('p', 'Memuat data admin...'));
  const data = await api('/admin');
  if (ticket !== rendering) return;
  heading('Admin Platform');
  for (const [title, items, label] of [
    ['Pengguna', data.users, item => `#${item.id} ${item.username}${item.is_platform_admin ? ' (admin)' : ''}${item.is_active ? '' : ' [nonaktif]'}`],
    ['Grup', data.groups, item => `#${item.id} ${item.name} — Leader: ${item.leader_username ? `#${item.leader_id} ${item.leader_username}` : '(belum ditetapkan)'}`],
    ['Koneksi database', data.connections, item => `#${item.id} ${item.name} (${item.db_user}@${item.host}:${item.port}/${item.db_name}, ssl=${item.ssl_mode})`],
    ['Izin', data.grants, item => `Grup #${item.group_id} ${item.group_name}: Database #${item.database_id} ${item.database_name} (${item.level})`],
  ]) {
    const section = node('section', '', { class: 'panel' });
    section.append(node('h2', title));
    const list = document.createElement('ul');
    for (const item of items || []) list.append(node('li', label(item)));
    section.append(list);
    main.append(section);
  }
  const members = node('section', '', { class: 'panel' });
  members.append(node('h2', 'Keanggotaan'));
  const memberList = document.createElement('ul');
  for (const item of data.memberships || []) memberList.append(node('li', `${item.username} (#${item.user_id}) - ${item.group_name} (#${item.group_id})`));
  members.append(memberList);
  main.append(members);

  const formsGrid = node('div', '', { class: 'card-list' });
  formsGrid.append(
    adminForm('Tambah pengguna', 'create_user', [['Username', 'username'], ['Password (min 12 karakter)', 'password', 'password'], ['Platform Admin', 'is_admin', 'checkbox', null, { required: false }]]),
    adminForm('Ubah pengguna', 'update_user', [['ID pengguna', 'user_id', 'number'], ['Username baru', 'username'], ['Platform Admin', 'is_admin', 'checkbox', null, { required: false }]]),
    adminForm('Status aktif pengguna', 'set_user_active', [['ID pengguna', 'user_id', 'number'], ['Aktif', 'active', 'checkbox', null, { required: false, value: true }]]),
    adminForm('Reset password pengguna', 'reset_password', [['ID pengguna', 'user_id', 'number'], ['Password baru (min 12 karakter)', 'password', 'password']]),
    adminForm('Hapus pengguna', 'delete_user', [['ID pengguna', 'user_id', 'number']]),
    adminForm('Tambah grup', 'create_group', [['Nama grup', 'name']]),
    adminForm('Ubah nama grup', 'rename_group', [['ID grup', 'group_id', 'number'], ['Nama grup baru', 'name']]),
    adminForm('Tetapkan leader grup', 'set_group_leader', [['ID grup', 'group_id', 'number'], ['ID user leader (0 / kosong untuk hapus)', 'leader_id', 'number', null, { required: false, allowZero: true }]]),
    adminForm('Hapus grup', 'delete_group', [['ID grup', 'group_id', 'number']]),
    adminForm('Tambah anggota', 'add_member', [['ID grup', 'group_id', 'number'], ['ID pengguna', 'user_id', 'number']]),
    adminForm('Keluarkan anggota', 'remove_member', [['ID grup', 'group_id', 'number'], ['ID pengguna', 'user_id', 'number']]),
    adminForm('Tambah koneksi', 'create_connection', [['Nama', 'name'], ['Host', 'host'], ['Port', 'port', 'number'], ['Nama database', 'db_name'], ['Pengguna database', 'db_user'], ['Password database', 'password', 'password'], ['SSL mode', 'ssl_mode', 'text', [['disable', 'disable'], ['prefer', 'prefer'], ['require', 'require']]]]),
    adminForm('Ubah koneksi', 'update_connection', [['ID database', 'database_id', 'number'], ['Nama', 'name'], ['Host', 'host'], ['Port', 'port', 'number'], ['Nama database', 'db_name'], ['Pengguna database', 'db_user'], ['Password baru (opsional)', 'password', 'password', null, { required: false }], ['SSL mode', 'ssl_mode', 'text', [['disable', 'disable'], ['prefer', 'prefer'], ['require', 'require']]]]),
    adminForm('Hapus koneksi', 'delete_connection', [['ID database', 'database_id', 'number']]),
    adminForm('Beri izin', 'grant', [['ID grup', 'group_id', 'number'], ['ID database', 'database_id', 'number'], ['Tingkat izin', 'level', 'text', [['VIEW_ONLY', 'VIEW_ONLY'], ['VIEW_EDIT', 'VIEW_EDIT']]]]),
    adminForm('Cabut izin', 'revoke', [['ID grup', 'group_id', 'number'], ['ID database', 'database_id', 'number']]),
  );
  main.append(formsGrid);
}

async function render() {
  const ticket = ++rendering;
  message('');
  main.replaceChildren(node('p', 'Memuat...'));
  renderNavigation();
  try {
    session = await api('/auth/session');
    setCSRF(session.csrf_token);
  } catch (error) {
    if (error.status !== 401) {
      heading('Layanan tidak tersedia');
      main.append(node('p', error.message));
      return;
    }
    session = null;
    setCSRF('');
  }
  if (ticket !== rendering) return;
  renderNavigation();
  if (!session) {
    if (location.pathname !== '/login') navigate('/login', true);
    else renderLogin();
    return;
  }
  const path = location.pathname;
  if (path === '/login' || path === '/') {
    navigate('/dashboard', true);
    return;
  }
  try {
    if (path === '/dashboard') renderDashboard();
    else if (path === '/databases') await renderDatabases(ticket);
    else if (/^\/databases\/\d+$/.test(path)) await renderDatabase(path.split('/')[2] + location.search, ticket);
    else if (/^\/databases\/\d+\/revisions$/.test(path)) await renderRevisions(path.split('/')[2], ticket);
    else if (/^\/databases\/\d+\/reviews$/.test(path)) await renderReviews(path.split('/')[2], ticket);
    else if (/^\/databases\/\d+\/history$/.test(path)) await renderHistory(path.split('/')[2], ticket);
    else if (/^\/jobs\/[A-Za-z0-9_-]+$/.test(path)) await renderJob(path.split('/')[2], ticket);
    else if (path === '/admin') await renderAdmin(ticket);
    else {
      heading('Halaman tidak ditemukan');
      main.append(link('Kembali ke dashboard', '/dashboard'));
    }
    if (ticket === rendering) main.focus();
  } catch (error) {
    if (ticket !== rendering) return;
    if (error.status === 401) {
      session = null;
      navigate('/login', true);
      return;
    }
    heading(error.status === 403 ? 'Akses ditolak' : 'Gagal memuat halaman');
    main.append(node('p', error.message));
    main.focus();
  }
}

onRoute(render);
render();
