// Popup script for webRequest test extension
document.addEventListener('DOMContentLoaded', () => {
  chrome.runtime.sendMessage({ action: 'ping' }, (response) => {
    const statusDiv = document.getElementById('status');
    if (response && response.status === 'pong') {
      statusDiv.textContent = 'Service worker active!';
      statusDiv.style.color = 'green';
    } else {
      statusDiv.textContent = 'Service worker not responding';
      statusDiv.style.color = 'red';
    }
  });
});
