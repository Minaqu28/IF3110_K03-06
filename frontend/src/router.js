const listeners = new Set();
export function onRoute(listener) { listeners.add(listener); return () => listeners.delete(listener); }
export function navigate(path, replace = false) {
  const target = new URL(path, location.origin);
  if (target.origin !== location.origin) return;
  history[replace ? 'replaceState' : 'pushState']({}, '', target.pathname + target.search);
  for (const listener of listeners) listener();
}
window.addEventListener('popstate', () => { for (const listener of listeners) listener(); });
document.addEventListener('click', event => {
  const link = event.target.closest('a[data-link]');
  if (!link || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  if (link.origin !== location.origin) return;
  event.preventDefault();
  navigate(link.pathname + link.search);
});
