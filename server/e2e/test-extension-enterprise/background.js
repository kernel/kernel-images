// Minimal enterprise extension background script
// This extension exists to test enterprise policy installation via ExtensionInstallForcelist.
// The webRequest permission requires enterprise policy for installation.

chrome.webRequest.onBeforeRequest.addListener(
  (details) => {
    // No-op listener - just to validate the extension loaded correctly
    return {};
  },
  { urls: ["<all_urls>"] }
);

console.log("Minimal Enterprise Test Extension loaded");
