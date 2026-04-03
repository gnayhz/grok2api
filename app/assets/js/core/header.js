/* Unified header injection — handles both admin and function pages */
(function() {
  const GITHUB_ICON = '<svg viewBox="0 0 24 24"><path d="M12 2C6.48 2 2 6.58 2 12.26c0 4.5 2.87 8.32 6.84 9.67.5.1.68-.22.68-.48 0-.24-.01-.86-.01-1.69-2.78.62-3.37-1.37-3.37-1.37-.45-1.17-1.11-1.48-1.11-1.48-.91-.64.07-.63.07-.63 1.01.07 1.54 1.06 1.54 1.06.9 1.58 2.36 1.12 2.94.86.09-.67.35-1.12.63-1.38-2.22-.26-4.56-1.14-4.56-5.07 0-1.12.39-2.04 1.03-2.76-.1-.26-.45-1.3.1-2.71 0 0 .84-.27 2.75 1.05A9.3 9.3 0 0 1 12 6.79c.85 0 1.7.12 2.5.36 1.9-1.32 2.74-1.05 2.74-1.05.55 1.41.2 2.45.1 2.71.64.72 1.03 1.64 1.03 2.76 0 3.94-2.34 4.81-4.58 5.06.36.32.68.95.68 1.92 0 1.38-.01 2.49-.01 2.83 0 .26.18.58.69.48A10.05 10.05 0 0 0 22 12.26C22 6.58 17.52 2 12 2Z"/></svg>';

  var ADMIN_NAV = `
    <a href="/admin/token" class="nav-link" data-nav="/admin/token" data-i18n="nav.tokenManage">Token管理</a>
    <a href="/admin/config" class="nav-link" data-nav="/admin/config" data-i18n="nav.configManage">配置管理</a>
    <a href="/admin/cache" class="nav-link" data-nav="/admin/cache" data-i18n="nav.cacheManage">缓存管理</a>`;

  var ADMIN_RIGHT = `
    <button id="lang-toggle" type="button" class="header-btn">EN</button>
    <button id="storage-mode-btn" type="button" class="header-btn">-</button>
    <a href="https://github.com/chenyme/grok2api/issues" target="_blank" class="header-btn" data-i18n="nav.feedback">反馈</a>
    <button onclick="logout()" type="button" class="header-btn" data-i18n="nav.logout">退出</button>`;

  var FUNCTION_NAV = `
    <a href="/chat" class="nav-link" data-nav="/chat">Chat</a>
    <a href="/imagine" class="nav-link" data-nav="/imagine">Imagine</a>
    <a href="/voice" class="nav-link" data-nav="/voice">Voice</a>
    <a href="/video" class="nav-link" data-nav="/video">Video</a>`;

  var FUNCTION_RIGHT = `
    <button id="lang-toggle" class="header-btn">EN</button>
    <button onclick="functionLogout()" class="header-btn" data-i18n="nav.logout">退出</button>`;

  var FOOTER_HTML = '<footer class="app-footer"><a href="https://github.com/chenyme/grok2api" target="_blank">Grok2API</a> Created By <a href="https://github.com/chenyme" target="_blank">@Chenyme</a></footer>';

  var container = document.getElementById('app-header');
  if (container && !container.children.length) {
    var type = container.dataset.type || 'admin';
    var isAdmin = type === 'admin';
    container.innerHTML = '<header class="app-header"><div class="app-header-inner">' +
      '<div class="header-left">' +
        '<a href="' + (isAdmin ? 'https://github.com/chenyme/grok2api' : '/') + '" ' + (isAdmin ? 'target="_blank" ' : '') + 'class="brand">' + GITHUB_ICON + '<span>Grok2API</span></a>' +
        '<div class="header-sep"></div>' +
        (isAdmin ? ADMIN_NAV : FUNCTION_NAV) +
      '</div>' +
      '<div class="header-right">' +
        (isAdmin ? ADMIN_RIGHT : FUNCTION_RIGHT) +
      '</div>' +
    '</div></header>';
  }

  // Inject footer if placeholder exists
  var footer = document.getElementById('app-footer');
  if (footer && !footer.children.length) {
    footer.innerHTML = FOOTER_HTML;
  }

  // Highlight active nav link
  var path = location.pathname;
  document.querySelectorAll('a[data-nav]').forEach(function(a) {
    if (path === a.dataset.nav || (a.dataset.nav !== '/' && path.startsWith(a.dataset.nav))) {
      a.classList.add('active');
    }
  });

  // Storage mode badge (admin only)
  var btn = document.getElementById('storage-mode-btn');
  if (btn && typeof fetchStorageType === 'function') {
    (async function() {
      btn.textContent = '...';
      var type = await fetchStorageType();
      var label = formatStorageLabel(type);
      btn.textContent = label === '-' ? '-' : label.toUpperCase();
      if (label !== '-') btn.classList.add('ready');
    })();
  }

  // Lang toggle
  var langBtn = document.getElementById('lang-toggle');
  if (langBtn) {
    langBtn.textContent = (window.I18n && I18n.getLang() === 'zh') ? 'EN' : '中';
    langBtn.addEventListener('click', function() { if (window.I18n) I18n.toggleLang(); });
  }
})();
