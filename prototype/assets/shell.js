/* Relay Deploy - shared prototype behaviour
   Scroll reveal, command palette, toasts, table filtering, tabs. */
(function(){
  'use strict';
  var reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  function reveal(){
    var items = document.querySelectorAll('.reveal');
    if (reduce || !('IntersectionObserver' in window)) {
      items.forEach(function(el){ el.classList.add('in'); });
      return;
    }
    var io = new IntersectionObserver(function(entries){
      entries.forEach(function(e, i){
        if (!e.isIntersecting) return;
        setTimeout(function(){ e.target.classList.add('in'); }, i * 60);
        io.unobserve(e.target);
      });
    }, { threshold: 0.12, rootMargin: '0px 0px -40px 0px' });
    items.forEach(function(el){ io.observe(el); });

    /* Safety net: if the reader jumps (End key, anchor, restored scroll)
       the observer never sees the skipped blocks. Reveal anything already
       at or above the fold on every scroll settle. */
    var pending = false;
    window.addEventListener('scroll', onScroll, true);
    function onScroll(){
      if (pending) return;
      pending = true;
      window.requestAnimationFrame(sweep);
    }
    function sweep(){
      pending = false;
      var left = document.querySelectorAll('.reveal:not(.in)');
      for (var i = 0; i < left.length; i++) {
        if (left[i].getBoundingClientRect().top < window.innerHeight) {
          left[i].classList.add('in');
          io.unobserve(left[i]);
        }
      }
    }
  }

  var X = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>';
  function toastHost(){
    var host = document.querySelector('.toasts');
    if (!host) {
      host = document.createElement('div');
      host.className = 'toasts';
      host.setAttribute('role', 'status');
      host.setAttribute('aria-live', 'polite');
      document.body.appendChild(host);
    }
    return host;
  }
  function toast(title, detail, kind){
    var host = toastHost();
    var el = document.createElement('div');
    el.className = 'toast' + (kind ? ' ' + kind : '');
    var b = document.createElement('b'); b.textContent = title;
    var s = document.createElement('span'); s.textContent = detail || '';
    var wrap = document.createElement('div'); wrap.appendChild(b); if (detail) wrap.appendChild(s);
    var close = document.createElement('button');
    close.type = 'button'; close.setAttribute('aria-label', 'Dismiss notification');
    close.innerHTML = X;
    el.appendChild(wrap); el.appendChild(close);
    host.appendChild(el);
    function bye(){
      if (!el.parentNode) return;
      el.classList.add('out');
      setTimeout(function(){ if (el.parentNode) el.parentNode.removeChild(el); }, 200);
    }
    close.addEventListener('click', bye);
    setTimeout(bye, kind === 'bad' ? 8000 : 5000);
  }

  /* command rows: group, label, sub, href, shortcut, action */
  var COMMANDS = [
    ['Navigate','Overview','Deploy health, last 7 days','./home.html','G H',''],
    ['Navigate','Features','14 platform capabilities','./features.html','G F',''],
    ['Navigate','Pricing and usage','Team plan, 84 dollars a month','./pricing.html','G P',''],
    ['Navigate','Docs','Rolling back a deployment','./docs.html','G D',''],
    ['Navigate','Contact support','3 open tickets','./contact.html','G C',''],
    ['Deployments','Build #4821','main a91fc3d Ready','','','inspect'],
    ['Deployments','Build #4819','fix/webhook-retry Failed','','','inspect'],
    ['Deployments','Build #4816','main promoted to production','','','inspect'],
    ['Actions','Promote build to production','Re-point the alias, no rebuild','','','promote'],
    ['Actions','Roll back acme.io','Median rollback 11 seconds','','','rollback'],
    ['Actions','Open a support ticket','First response 1 business day','./contact.html','',''],
    ['Actions','Download invoice CSV','12 invoices, Aug 2025 to Jul 2026','','','invoice']
  ];
  var ARROW = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M5 12h14M13 6l6 6-6 6"/></svg>';

  function palette(){
    var root = document.getElementById('cmdk');
    if (!root) return;
    var input = root.querySelector('#cmdk-input');
    var list  = root.querySelector('#cmdk-list');
    var idx = 0, shown = [], lastFocus = null;

    function render(q){
      q = (q || '').trim().toLowerCase();
      shown = COMMANDS.filter(function(c){
        return !q || (c[1] + ' ' + c[2] + ' ' + c[0]).toLowerCase().indexOf(q) > -1;
      });
      list.innerHTML = '';
      if (!shown.length) {
        var none = document.createElement('li');
        none.className = 'cmdk-none';
        none.textContent = 'No match. Try rollback, invoice, or docs.';
        list.appendChild(none);
        return;
      }
      idx = Math.min(idx, shown.length - 1);
      var group = null;
      shown.forEach(function(c, i){
        if (c[0] !== group) {
          group = c[0];
          var h = document.createElement('li');
          h.className = 'cmdk-group';
          h.textContent = group;
          h.setAttribute('role', 'presentation');
          list.appendChild(h);
        }
        var li = document.createElement('li');
        li.setAttribute('role', 'presentation');
        var b = document.createElement('button');
        b.type = 'button';
        b.className = 'cmdk-item';
        b.id = 'cmdk-o' + i;
        b.setAttribute('role', 'option');
        b.setAttribute('aria-selected', i === idx ? 'true' : 'false');
        var kb = c[4] ? '<kbd class="shortcut">' + c[4] + '</kbd>' : '';
        b.innerHTML = ARROW + '<span>' + c[1] + '</span><span class="sub">' + c[2] + '</span>' + kb;
        b.addEventListener('click', bindRun(c));
        b.addEventListener('mousemove', bindHover(i));
        li.appendChild(b);
        list.appendChild(li);
      });
      mark();
    }
    function bindRun(c){
      return function(){ run(c); };
    }
    function bindHover(i){
      return function(){ idx = i; mark(); };
    }
    function mark(){
      var opts = list.querySelectorAll('.cmdk-item');
      for (var i = 0; i < opts.length; i++) {
        opts[i].setAttribute('aria-selected', i === idx ? 'true' : 'false');
      }
      if (opts[idx]) {
        input.setAttribute('aria-activedescendant', opts[idx].id);
        opts[idx].scrollIntoView(false);
      }
    }
    function run(c){
      close();
      if (c[3]) {
        window.location.href = c[3];
        return;
      }
      var a = c[5];
      if (a === 'promote') {
        toast('Build #4821 promoted', 'Alias acme.io re-pointed to dpl_4821 in 9s. Health check passed.', 'ok');
        return;
      }
      if (a === 'rollback') {
        toast('Rolled back to #4816', 'acme.io is serving the previous artifact. No build minutes consumed.', 'ok');
        return;
      }
      if (a === 'invoice') {
        toast('CSV queued', '12 invoices emailed to arnab at acme.io within 2 minutes.', '');
        return;
      }
      toast(c[1], c[2], '');
    }
    function open(){
      lastFocus = document.activeElement;
      root.classList.add('open');
      root.setAttribute('aria-hidden', 'false');
      input.value = '';
      idx = 0;
      render('');
      input.focus();
    }
    function close(){
      root.classList.remove('open');
      root.setAttribute('aria-hidden', 'true');
      if (lastFocus && lastFocus.focus) lastFocus.focus();
    }
    window.RelayPalette = Object.create(null);
    window.RelayPalette.open = open;
    window.RelayPalette.close = close;

    input.addEventListener('input', onType);
    function onType(){
      idx = 0;
      render(input.value);
    }
    input.addEventListener('keydown', onNavKey);
    function onNavKey(e){
      var k = e.key;
      if (k === 'ArrowDown') {
        e.preventDefault();
        idx = Math.min(idx + 1, shown.length - 1);
        mark();
        return;
      }
      if (k === 'ArrowUp') {
        e.preventDefault();
        idx = Math.max(idx - 1, 0);
        mark();
        return;
      }
      if (k === 'Enter') {
        e.preventDefault();
        if (shown[idx]) run(shown[idx]);
        return;
      }
      if (k === 'Escape') {
        e.preventDefault();
        close();
      }
    }
    root.addEventListener('mousedown', onBackdrop);
    function onBackdrop(e){
      if (e.target === root) close();
    }
    document.addEventListener('keydown', onGlobalKey);
    function onGlobalKey(e){
      var k = (e.key || '').toLowerCase();
      var isOpen = root.classList.contains('open');
      if ((e.metaKey || e.ctrlKey) && k === 'k') {
        e.preventDefault();
        if (isOpen) close();
        else open();
        return;
      }
      if (k === 'escape' && isOpen) {
        close();
        return;
      }
      if (k === '/' && !isOpen) {
        var t = e.target.tagName;
        if (t !== 'INPUT' && t !== 'TEXTAREA' && t !== 'SELECT') {
          e.preventDefault();
          open();
        }
      }
    }
    var triggers = document.querySelectorAll('[data-cmdk]');
    for (var t = 0; t < triggers.length; t++) {
      triggers[t].addEventListener('click', onTrigger);
    }
    function onTrigger(e){
      e.preventDefault();
      open();
    }
  }

  /* generic wiring shared by every page */
  function wiring(){
    var i;

    var announcers = document.querySelectorAll('[data-toast]');
    for (i = 0; i < announcers.length; i++) {
      announcers[i].addEventListener('click', onAnnounce);
    }
    function onAnnounce(e){
      var el = e.currentTarget;
      if (el.tagName === 'A') e.preventDefault();
      toast(el.getAttribute('data-toast'),
            el.getAttribute('data-toast-detail') || '',
            el.getAttribute('data-toast-kind') || '');
    }

    var segs = document.querySelectorAll('.seg');
    for (i = 0; i < segs.length; i++) {
      segs[i].addEventListener('click', onSeg);
    }
    function onSeg(e){
      var seg = e.currentTarget;
      var b = e.target.closest('button');
      if (!b) return;
      var all = seg.querySelectorAll('button');
      for (var j = 0; j < all.length; j++) {
        all[j].setAttribute('aria-pressed', all[j] === b ? 'true' : 'false');
      }
      var name = seg.getAttribute('data-seg');
      if (name) document.dispatchEvent(new CustomEvent(name, RelayDetail(b.getAttribute('data-value'))));
    }

    var tabsets = document.querySelectorAll('[data-tabs]');
    for (i = 0; i < tabsets.length; i++) {
      tabsets[i].addEventListener('click', onTabClick);
      tabsets[i].addEventListener('keydown', onTabKey);
    }
    function onTabClick(e){
      var tabs = e.currentTarget;
      var b = e.target.closest('[data-filter]');
      if (!b) return;
      applyFilter(tabs, b);
    }
    function applyFilter(tabs, b){
      var scope = document.querySelector(tabs.getAttribute('data-tabs'));
      if (!scope) return;
      var f = b.getAttribute('data-filter');
      var all = tabs.querySelectorAll('[data-filter]');
      for (var j = 0; j < all.length; j++) {
        all[j].setAttribute('aria-selected', all[j] === b ? 'true' : 'false');
      }
      var cards = scope.querySelectorAll('[data-cat]');
      var n = 0;
      for (var c = 0; c < cards.length; c++) {
        var cats = cards[c].getAttribute('data-cat').split(' ');
        var hit = f === 'all' || cats.indexOf(f) > -1;
        cards[c].hidden = !hit;
        if (hit) n++;
      }
      var live = document.getElementById(tabs.getAttribute('data-live') || '');
      if (live) live.textContent = n + ' shown';
    }
    function onTabKey(e){
      if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
      var tabs = e.currentTarget;
      var all = Array.prototype.slice.call(tabs.querySelectorAll('[data-filter]'));
      var at = all.indexOf(document.activeElement);
      if (at < 0) return;
      e.preventDefault();
      var step = e.key === 'ArrowRight' ? 1 : all.length - 1;
      var next = all[(at + step) % all.length];
      next.focus();
      applyFilter(tabs, next);
    }

    var filters = document.querySelectorAll('[data-filters]');
    for (i = 0; i < filters.length; i++) {
      filters[i].addEventListener('input', onFilterType);
    }
    function onFilterType(e){
      var input = e.currentTarget;
      var table = document.querySelector(input.getAttribute('data-filters'));
      if (!table) return;
      var q = input.value.trim().toLowerCase();
      var rows = table.querySelectorAll('tbody tr');
      var n = 0;
      for (var j = 0; j < rows.length; j++) {
        var hit = !q || rows[j].textContent.toLowerCase().indexOf(q) > -1;
        rows[j].hidden = !hit;
        if (hit) n++;
      }
      var sel = input.getAttribute('data-empty');
      var empty = sel ? document.querySelector(sel) : null;
      if (empty) empty.hidden = n !== 0;
      var cs = input.getAttribute('data-count');
      var countEl = cs ? document.querySelector(cs) : null;
      if (countEl) countEl.textContent = String(n);
    }

    var copiers = document.querySelectorAll('[data-copy]');
    for (i = 0; i < copiers.length; i++) {
      copiers[i].addEventListener('click', onCopy);
    }
    function onCopy(e){
      var btn = e.currentTarget;
      var src = document.querySelector(btn.getAttribute('data-copy'));
      var txt = src ? src.innerText : '';
      if (navigator.clipboard) navigator.clipboard.writeText(txt).catch(noop);
      toast('Copied to clipboard', txt.split(String.fromCharCode(10))[0].slice(0, 64), 'ok');
    }
    function noop(){}
  }

  function RelayDetail(v){
    var o = Object.create(null);
    o.detail = v;
    return o;
  }

  function boot(){
    reveal();
    palette();
    wiring();
  }
  window.Relay = Object.create(null);
  window.Relay.toast = toast;
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
