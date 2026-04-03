window.renderAdminHeader = async function renderAdminHeader() {
  const mount = document.getElementById('admin-header');
  if (!mount || mount.children.length) return;

  try {
    const res = await fetch('/static/admin/header.html', { cache: 'no-store' });
    if (!res.ok) throw new Error('header unavailable');
    mount.innerHTML = await res.text();
  } catch {
    mount.innerHTML = `
      <header class="admin-header">
        <div class="admin-header-inner">
          <div class="admin-brand-wrap">
            <span class="admin-brand">Grok2API</span>
          </div>
          <nav class="admin-nav">
            <a href="/admin/account" class="admin-nav-link" data-nav="/admin/account">账户管理</a>
            <a href="/admin/config" class="admin-nav-link" data-nav="/admin/config">配置管理</a>
          </nav>
          <div class="admin-header-right">
            <span class="admin-badge admin-badge-green" id="hd-backend">—</span>
            <button onclick="adminLogout()" class="btn btn-ghost btn-sm">退出</button>
          </div>
        </div>
      </header>`;
  }

  const active = mount.dataset.active || location.pathname;
  mount.querySelectorAll('[data-nav]').forEach((link) => {
    link.classList.toggle('active', link.dataset.nav === active);
  });

  const inner = mount.querySelector('.admin-header-inner');
  if (inner) {
    inner.style.maxWidth = '1280px';
    inner.style.width = '100%';
    inner.style.height = '54px';
    inner.style.margin = '0 auto';
    inner.style.padding = '0 28px';
    inner.style.display = 'flex';
    inner.style.alignItems = 'center';
    inner.style.justifyContent = 'space-between';
    inner.style.flexWrap = 'nowrap';
  }
};
