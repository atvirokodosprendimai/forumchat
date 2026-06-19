// timer.js — client-side ticking for the personal worklog timer.
//
// The server owns the start time (timer_sessions.started_at); the browser only
// renders the elapsed delta. The running widget carries
//   <span id="timer-elapsed" data-timer-start="<unix-seconds>">
// and a data-init that calls window.fcTimerTick() each time it morphs in.
// A single interval updates the text once per second and self-clears when the
// element is gone (i.e. the widget morphed back to idle/note state).
(function () {
  let interval = null;

  function pad(n) {
    return String(n).padStart(2, "0");
  }

  function fmt(totalSeconds) {
    const h = Math.floor(totalSeconds / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    const s = totalSeconds % 60;
    return pad(h) + ":" + pad(m) + ":" + pad(s);
  }

  function tick() {
    const el = document.getElementById("timer-elapsed");
    if (!el) {
      if (interval) {
        clearInterval(interval);
        interval = null;
      }
      return;
    }
    const start = parseInt(el.getAttribute("data-timer-start"), 10);
    if (!start) {
      return;
    }
    const now = Math.floor(Date.now() / 1000);
    el.textContent = fmt(Math.max(0, now - start));
  }

  window.fcTimerTick = function () {
    if (interval) {
      clearInterval(interval);
    }
    tick();
    interval = setInterval(tick, 1000);
  };
})();
