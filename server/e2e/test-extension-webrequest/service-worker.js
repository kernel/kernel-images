// Service worker for webRequest test extension
console.log('WebRequest test extension service worker loaded');

// Listen for messages from popup
chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message.action === 'ping') {
    sendResponse({ status: 'pong', timestamp: Date.now() });
    return true;
  }
});

// Simple webRequest listener (doesn't block, just observes)
chrome.webRequest.onBeforeRequest.addListener(
  (details) => {
    console.log('Request observed:', details.url);
  },
  { urls: ['https://example.com/*'] }
);
