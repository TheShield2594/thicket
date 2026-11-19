const params = new URLSearchParams(location.search);
const server = params.get('server') || 'your server';
const reason = params.get('reason');
document.getElementById('detail').textContent =
  `${server} isn’t answering${reason ? ` (${reason})` : ''}. Check that Thicket is running and this computer can reach it.`;
document.getElementById('retry').addEventListener('click', () => window.thicketDesktop.retry());
document.getElementById('change').addEventListener('click', () => window.thicketDesktop.changeServer());
// Try again by itself every 15 seconds.
setInterval(() => window.thicketDesktop.retry(), 15000);
