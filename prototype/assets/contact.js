/* Contact page - ticket form validation and attachment chips */
(function(){
  'use strict';
  var form = document.getElementById('ticket-form');
  if (!form) return;

  var subject = document.getElementById('t-subject');
  var body = document.getElementById('t-body');
  var drop = document.getElementById('t-drop');
  var file = document.getElementById('t-file');
  var chips = document.getElementById('t-chips');
  var seq = 3183;

  function setErr(field, id, bad){
    field.setAttribute('aria-invalid', bad ? 'true' : 'false');
    var e = document.getElementById(id);
    if (e) e.classList.toggle('show', bad);
  }

  subject.addEventListener('input', clearSubject);
  function clearSubject(){
    if (subject.value.trim()) setErr(subject, 't-subject-err', false);
  }
  body.addEventListener('input', clearBody);
  function clearBody(){
    if (body.value.trim().length >= 20) setErr(body, 't-body-err', false);
  }

  form.addEventListener('submit', onSubmit);
  function onSubmit(e){
    e.preventDefault();
    var badSubject = !subject.value.trim();
    var badBody = body.value.trim().length < 20;
    setErr(subject, 't-subject-err', badSubject);
    setErr(body, 't-body-err', badBody);
    if (badSubject || badBody) {
      (badSubject ? subject : body).focus();
      window.Relay.toast('Ticket not sent', 'Two fields still need attention before support can triage this.', 'bad');
      return;
    }
    var sev = document.getElementById('t-severity').value.slice(0, 2);
    var id = 'SUP-' + seq;
    seq += 1;
    form.reset();
    if (chips) chips.innerHTML = '';
    window.Relay.toast(id + ' created', sev + ' on acme-platform. First response target 1 business day.', 'ok');
  }

  if (!drop || !file) return;
  drop.addEventListener('dragover', onOver);
  drop.addEventListener('dragleave', onLeave);
  drop.addEventListener('drop', onDrop);
  file.addEventListener('change', onPick);

  function onOver(e){
    e.preventDefault();
    drop.classList.add('over');
  }
  function onLeave(){
    drop.classList.remove('over');
  }
  function onDrop(e){
    e.preventDefault();
    drop.classList.remove('over');
    add(e.dataTransfer && e.dataTransfer.files);
  }
  function onPick(){
    add(file.files);
  }
  function add(list){
    if (!list) return;
    for (var i = 0; i < list.length; i++) {
      chip(list[i].name, list[i].size);
    }
  }
  function chip(name, size){
    var kb = Math.max(1, Math.round((size || 0) / 1024));
    var el = document.createElement('span');
    el.className = 'chip';
    var label = document.createElement('span');
    label.textContent = name + ' ' + kb + ' KB';
    var x = document.createElement('button');
    x.type = 'button';
    x.setAttribute('aria-label', 'Remove ' + name);
    x.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>';
    x.addEventListener('click', drop_chip);
    function drop_chip(){
      el.remove();
    }
    el.appendChild(label);
    el.appendChild(x);
    chips.appendChild(el);
  }
})();
