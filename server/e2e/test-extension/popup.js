document.getElementById('pingBtn').addEventListener('click', async () => {
  const statusEl = document.getElementById('status');
  statusEl.textContent = 'Sending ping to service worker...';
  statusEl.className = '';
  try {
    const response = await chrome.runtime.sendMessage({ type: 'ping' });
    if (response) {
      statusEl.textContent = `SUCCESS: ${response.message} (timestamp: ${response.timestamp})`;
      statusEl.className = 'success';
    } else {
      statusEl.textContent = 'ERROR: No response from service worker';
      statusEl.className = 'error';
    }
  } catch (error) {
    statusEl.textContent = `ERROR: ${error.message}`;
    statusEl.className = 'error';
  }
});
