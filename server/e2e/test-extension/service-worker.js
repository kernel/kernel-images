// MV3 Service Worker Test
console.log('[MV3 Test] Service worker starting...');

chrome.runtime.onInstalled.addListener((details) => {
  console.log('[MV3 Test] Extension installed:', details.reason);
  chrome.storage.local.set({ installTime: Date.now(), reason: details.reason });
});

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  console.log('[MV3 Test] Received message:', message);
  if (message.type === 'ping') {
    sendResponse({ status: 'pong', timestamp: Date.now(), message: 'Service worker is alive!' });
  }
  return true;
});

console.log('[MV3 Test] Service worker initialized at', new Date().toISOString());
