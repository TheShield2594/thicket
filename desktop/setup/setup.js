const params = new URLSearchParams(location.search);
const input = document.getElementById('server');
const error = document.getElementById('error');
const go = document.getElementById('go');
input.value = params.get('server') || '';

document.getElementById('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  error.textContent = '';
  go.disabled = true;
  go.textContent = 'Connecting…';
  const res = await window.thicketDesktop.connect(input.value);
  if (!res.ok) {
    error.textContent = res.error;
    go.disabled = false;
    go.textContent = 'Connect';
  }
});
