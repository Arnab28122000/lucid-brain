/* Pricing page - billing cycle toggle */
(function(){
  'use strict';
  var SEATS = 4;
  var note = document.getElementById('cycle-note');
  var total = document.getElementById('plan-total');
  var seat = document.getElementById('plan-seat');
  var prices = document.querySelectorAll('[data-price-monthly]');

  document.addEventListener('cycle', apply);

  function apply(e){
    var annual = e.detail === 'annual';
    var attr = annual ? 'data-price-annual' : 'data-price-monthly';
    for (var i = 0; i < prices.length; i++) {
      prices[i].textContent = prices[i].getAttribute(attr);
    }
    if (annual) {
      if (seat) seat.textContent = '$17.50';
      if (total) total.textContent = '$70';
      if (note) note.textContent = 'Billed $840 on 28 Aug 2026. You save $168 a year at ' + SEATS + ' seats.';
      if (window.Relay) window.Relay.toast('Switched to annual', 'Team drops to $17.50 per seat. Change applies on renewal, 28 Aug 2026.', 'ok');
      return;
    }
    if (seat) seat.textContent = '$21';
    if (total) total.textContent = '$84';
    if (note) note.textContent = 'Switching to annual saves $168 a year at ' + SEATS + ' seats.';
  }
})();
