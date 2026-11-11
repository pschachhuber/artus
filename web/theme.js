// Applies the saved theme before the first paint. The theme choice is the
// only thing Artus keeps in the browser; everything else lives on the server.
try {
  const saved = localStorage.getItem('artus-theme');
  if (saved === 'light' || saved === 'dark') document.documentElement.dataset.theme = saved;
} catch { /* storage unavailable: follow the system */ }
