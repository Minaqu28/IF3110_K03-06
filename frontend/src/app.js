import { api, ApiError, setCSRF } from './api.js';
import { navigate, onRoute } from './router.js';

const main = document.querySelector('#main');
const nav = document.querySelector('#navigation');
const notice = document.querySelector('#notice');
let session = null;
let rendering = 0;

function node(tag, text, attributes = {}) {
  const element = document.createElement(tag);
  element.textContent = text;
  for (const [name, value] of Object.entries(attributes)) element.setAttribute(name, value);
  return element;
}
function link(text, href) { const a = node('a', text, { href, 'data-link': '' }); return a; }
function message(text) { notice.textContent = text; }
function heading(text) { document.title = `${text} · DBMP`; main.replaceChildren(node('h1', text)); }
function field(form, label, name, type = 'text') {
  const id = `field-${name}`;
  const wrapper = node('div', '', { class: 'field' });
  wrapper.append(node('label', label, { for: id }));
  const input = document.createElement('input');
  input.id = id; input.name = name; input.type = type; input.required = true;
  if (name === 'username') input.autocomplete = 'username';
  if (name === 'password') input.autocomplete = 'current-password';
  wrapper.append(input); form.append(wrapper);
  return input;
}
function adminForm(title, action, fields) {
  const form = document.createElement('form');
  form.append(node('h2', title));
  const inputs = {};
  for (const [label, name, type = 'text', options] of fields) {
    if (options) {
      const wrapper = node('div', '', { class: 'field' });
      const id = `admin-${action}-${name}`;
      wrapper.append(node('label', label, { for: id }));
      const select = node('select', '', { id, name, required: '' });
      for (const [value, text] of options) select.append(node('option', text, { value }));
      wrapper.append(select); form.append(wrapper); inputs[name] = select;
    } else {
      const input = field(form, label, `${action}-${name}`, type);
      input.name = name;
      if (type === 'number') input.min = '1';
      inputs[name] = input;
    }
  }
  const error = node('p', '', { role: 'alert', class: 'error' });
  const submit = node('button', title, { type: 'submit' });
  form.append(error, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault(); submit.disabled = true; error.textContent = '';
    const body = { action };
    for (const [name, input] of Object.entries(inputs)) body[name] = input.type === 'number' ? Number(input.value) : input.value;
    try { await api('/admin/actions', { method: 'POST', body }); navigate('/admin', true); message(`${title} berhasil.`); }
    catch (failure) { error.textContent = failure.message; }
    finally { submit.disabled = false; }
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
    try { await api('/auth/logout', { method: 'POST' }); session = null; setCSRF(''); navigate('/login'); }
    catch (error) { message(error.message); }
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
  form.append(errorBox, submit); main.append(form);
  form.addEventListener('submit', async event => {
    event.preventDefault(); submit.disabled = true; errorBox.textContent = '';
    try {
      const { csrf_token } = await api('/auth/login-csrf');
      await api('/auth/login', { method: 'POST', csrf: csrf_token, body: { username: username.value, password: password.value } });
      session = await api('/auth/session'); setCSRF(session.csrf_token); navigate('/dashboard');
    } catch (error) { errorBox.textContent = error.message; }
    finally { submit.disabled = false; }
  });
  username.focus();
}
function renderDashboard() {
  heading('Dashboard');
  main.append(node('p', `Halo, ${session.user.username}.`), link('Lihat database yang dapat diakses', '/databases'));
}
async function renderDatabases(ticket) {
  heading('Database'); main.append(node('p', 'Memuat database...'));
  const databases = await api('/databases'); if (ticket !== rendering) return;
  heading('Database');
  const list = node('ul', '', { class: 'card-list' });
  for (const database of databases) {
    const item = document.createElement('li'); item.append(link(database.name, `/databases/${database.id}`)); list.append(item);
  }
  main.append(databases.length ? list : node('p', 'Belum ada database yang dapat diakses.'));
}
async function renderDatabase(id, ticket) {
  const databaseID = id.split('?')[0];
  heading('Database'); main.append(node('p', 'Memuat tabel...'));
  const view = await api(`/databases/${id}`); if (ticket !== rendering) return;
  heading(view.connection.name);
  const list = node('ul', '', { class: 'card-list' });
  for (const table of view.tables || []) {
    const item = document.createElement('li');
    const target = `/databases/${databaseID}?schema=${encodeURIComponent(table.schema)}&table=${encodeURIComponent(table.name)}`;
    item.append(link(`${table.schema}.${table.name}`, target)); list.append(item);
  }
  main.append(list);
  const features = node('nav', '', { 'aria-label': 'Fitur database' });
  features.append(link('Revisi migration', `/databases/${databaseID}/revisions`), document.createTextNode(' | '), link('Review', `/databases/${databaseID}/reviews`), document.createTextNode(' | '), link('Riwayat', `/databases/${databaseID}/history`));
  main.append(features);
  if (view.preview) {
    const scroll = node('div', '', { class: 'table-scroll', role: 'region', 'aria-label': 'Data tabel', tabindex: '0' });
    const table = document.createElement('table');
    const caption = node('caption', `Data ${view.table}`); table.append(caption);
    const head = document.createElement('thead'); const row = document.createElement('tr');
    for (const col of view.preview.columns || []) row.append(node('th', col, { scope: 'col' }));
    head.append(row); table.append(head);
    const body = document.createElement('tbody');
    for (const values of view.preview.rows || []) {
      const tr = document.createElement('tr');
      for (const cell of values) tr.append(node('td', cell.null ? 'NULL' : cell.value));
      body.append(tr);
    }
    table.append(body); scroll.append(table); main.append(scroll);
  }
}
async function renderDatabaseSection(databaseID, section, ticket) {
  const names = { revisions: 'Revisi migration', reviews: 'Review', history: 'Riwayat eksekusi' };
  heading(names[section]); main.append(node('p', 'Memuat...'));
  const items = await api(`/databases/${databaseID}/${section}`);
  if (ticket !== rendering) return;
  heading(names[section]); main.append(link('Kembali ke database', `/databases/${databaseID}`));
  const list = node('ul', '', { class: 'card-list' });
  for (const item of items || []) {
    const li = document.createElement('li');
    if (section === 'revisions') li.append(node('strong', `${item.title} (revisi ${item.revision})`), node('p', `Status: ${item.status}`));
    if (section === 'reviews') li.append(node('strong', `Review #${item.id}`), node('p', `Status: ${item.status}`));
    if (section === 'history') li.append(node('strong', `Attempt #${item.attempt_id}`), node('p', `Status: ${item.status}`));
    list.append(li);
  }
  main.append(list, items?.length ? document.createTextNode('') : node('p', 'Belum ada data.'));
}
async function renderJob(id, ticket) {
  heading('Status job'); main.append(node('p', 'Memuat status...'));
  const job = await api(`/jobs/${encodeURIComponent(id)}`);
  if (ticket !== rendering) return;
  heading('Status job');
  main.append(node('p', `Job ${job.id}: ${job.status}`));
  if (job.progress) main.append(node('p', `Batch selesai: ${job.progress.completed}${job.progress.total == null ? '' : ` dari ${job.progress.total}`}`));
  if (job.error_message) main.append(node('p', job.error_message, { role: 'alert' }));
  if (['QUEUED', 'RUNNING', 'RETRY_WAIT'].includes(job.status)) {
    main.append(node('p', 'Status akan diperbarui otomatis.'));
    setTimeout(() => { if (ticket === rendering) renderJob(id, ticket).catch(error => message(error.message)); }, 2000);
  }
}
async function renderAdmin(ticket) {
  if (!session.user.is_platform_admin) throw new ApiError(403, 'FORBIDDEN', 'Akses admin ditolak.');
  heading('Admin Platform'); main.append(node('p', 'Memuat data admin...'));
  const data = await api('/admin'); if (ticket !== rendering) return;
  heading('Admin Platform');
  for (const [title, items, label] of [
    ['Pengguna', data.users, item => `#${item.id} ${item.username}${item.is_platform_admin ? ' (admin)' : ''}`],
    ['Grup', data.groups, item => `#${item.id} ${item.name}`],
    ['Koneksi database', data.connections, item => `#${item.id} ${item.name}`],
    ['Izin', data.grants, item => `Grup #${item.group_id} ${item.group_name}: Database #${item.database_id} ${item.database_name} (${item.level})`],
  ]) {
    const section = node('section', '', { class: 'panel' }); section.append(node('h2', title));
    const list = document.createElement('ul');
    for (const item of items || []) list.append(node('li', label(item)));
    section.append(list); main.append(section);
  }
  const members = node('section', '', { class: 'panel' }); members.append(node('h2', 'Keanggotaan'));
  const memberList = document.createElement('ul');
  for (const item of data.memberships || []) memberList.append(node('li', `${item.username} - ${item.group_name}`));
  members.append(memberList); main.append(members);
  main.append(
    adminForm('Tambah pengguna', 'create_user', [['Username', 'username'], ['Password', 'password', 'password']]),
    adminForm('Tambah grup', 'create_group', [['Nama grup', 'name']]),
    adminForm('Tambah anggota', 'add_member', [['ID grup', 'group_id', 'number'], ['ID pengguna', 'user_id', 'number']]),
    adminForm('Tambah koneksi', 'create_connection', [['Nama', 'name'], ['Host', 'host'], ['Port', 'port', 'number'], ['Nama database', 'db_name'], ['Pengguna database', 'db_user'], ['Password database', 'password', 'password'], ['SSL mode', 'ssl_mode', 'text']]),
    adminForm('Beri izin', 'grant', [['ID grup', 'group_id', 'number'], ['ID database', 'database_id', 'number'], ['Tingkat izin', 'level', 'text', [['VIEW_ONLY', 'VIEW_ONLY'], ['VIEW_EDIT', 'VIEW_EDIT']]]]),
  );
}
async function render() {
  const ticket = ++rendering; message(''); main.replaceChildren(node('p', 'Memuat...')); renderNavigation();
  try {
    session = await api('/auth/session'); setCSRF(session.csrf_token);
  } catch (error) {
    if (error.status !== 401) { heading('Layanan tidak tersedia'); main.append(node('p', error.message)); return; }
    session = null; setCSRF('');
  }
  if (ticket !== rendering) return;
  renderNavigation();
  if (!session) { if (location.pathname !== '/login') navigate('/login', true); else renderLogin(); return; }
  const path = location.pathname;
  if (path === '/login' || path === '/') { navigate('/dashboard', true); return; }
  try {
    if (path === '/dashboard') renderDashboard();
    else if (path === '/databases') await renderDatabases(ticket);
    else if (/^\/databases\/\d+$/.test(path)) await renderDatabase(path.split('/')[2] + location.search, ticket);
    else if (/^\/databases\/\d+\/(revisions|reviews|history)$/.test(path)) await renderDatabaseSection(path.split('/')[2], path.split('/')[3], ticket);
    else if (/^\/jobs\/[A-Za-z0-9_-]+$/.test(path)) await renderJob(path.split('/')[2], ticket);
    else if (path === '/admin') await renderAdmin(ticket);
    else { heading('Halaman tidak ditemukan'); main.append(link('Kembali ke dashboard', '/dashboard')); }
    if (ticket === rendering) main.focus();
  } catch (error) {
    if (ticket !== rendering) return;
    if (error.status === 401) { session = null; navigate('/login', true); return; }
    heading(error.status === 403 ? 'Akses ditolak' : 'Gagal memuat halaman');
    main.append(node('p', error.message)); main.focus();
  }
}
onRoute(render); render();
