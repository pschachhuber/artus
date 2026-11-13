// Applies the saved theme before the first paint. The theme choice is the
// only thing Artus keeps in the browser; everything else lives on the server.
try {
  const saved = localStorage.getItem('artus-theme');
  if (saved === 'light' || saved === 'dark') document.documentElement.dataset.theme = saved;
} catch { /* storage unavailable: follow the system */ }

/** Colours the phone's status bar like the page: by the chosen theme, or the system's when none is chosen. */
function paintThemeColor() {
  const chosen = document.documentElement.dataset.theme;
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    const dark = chosen ? chosen === 'dark' : meta.media.includes('dark');
    meta.content = dark ? '#17181a' : '#fbfbfa';
  }
}
paintThemeColor();
