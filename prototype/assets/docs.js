/* Docs page - code tabs and table-of-contents scrollspy */
(function(){
  'use strict';

  var tabs = document.querySelectorAll('[data-code]');
  for (var i = 0; i < tabs.length; i++) {
    tabs[i].addEventListener('click', onTab);
  }
  function onTab(e){
    var b = e.currentTarget;
    var want = b.getAttribute('data-code');
    for (var j = 0; j < tabs.length; j++) {
      var key = tabs[j].getAttribute('data-code');
      tabs[j].setAttribute('aria-selected', key === want ? 'true' : 'false');
      var pane = document.getElementById('code-' + key);
      if (pane) pane.hidden = key !== want;
    }
  }

  var links = document.querySelectorAll('.toc a[href^="' + String.fromCharCode(35) + '"]');
  var targets = [];
  for (var k = 0; k < links.length; k++) {
    var el = document.getElementById(links[k].getAttribute('href').slice(1));
    if (el) targets.push([el, links[k]]);
  }
  if (!targets.length) return;

  var reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!('IntersectionObserver' in window)) return;

  var io = new IntersectionObserver(onSee, opts());
  function opts(){
    var o = Object.create(null);
    o.rootMargin = '-88px 0px -70% 0px';
    o.threshold = 0;
    return o;
  }
  function onSee(entries){
    for (var n = 0; n < entries.length; n++) {
      if (!entries[n].isIntersecting) continue;
      for (var m = 0; m < targets.length; m++) {
        targets[m][1].classList.toggle('on', targets[m][0] === entries[n].target);
      }
    }
  }
  for (var t = 0; t < targets.length; t++) {
    io.observe(targets[t][0]);
  }

  if (reduce) return;
  for (var L = 0; L < links.length; L++) {
    links[L].addEventListener('click', onJump);
  }
  function onJump(e){
    var id = e.currentTarget.getAttribute('href').slice(1);
    var el = document.getElementById(id);
    if (!el) return;
    e.preventDefault();
    var b = Object.create(null);
    b.behavior = 'smooth';
    b.block = 'start';
    el.scrollIntoView(b);
    history.replaceState(null, '', String.fromCharCode(35) + id);
  }
})();
