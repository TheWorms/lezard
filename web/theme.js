try {
  var t = localStorage.getItem('lezard.theme');
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
} catch (e) { /* stockage indisponible : thème par défaut */ }
