// Mobile left-drawer toggle. Sidebar visibility on >=900px is pure
// CSS — this script only handles the mobile slide-in: tap hamburger
// to open, tap overlay or any sidebar link to close. Toggles a
// `nav-open` class on <body> that the CSS keys off.
(() => {
  function setOpen(open) {
    document.body.classList.toggle('nav-open', open);
  }
  document.addEventListener('click', (ev) => {
    const t = ev.target;
    if (!(t instanceof Element)) return;
    if (t.closest('[data-nav-toggle]')) {
      ev.preventDefault();
      setOpen(!document.body.classList.contains('nav-open'));
      return;
    }
    if (t.closest('[data-nav-overlay]')) {
      setOpen(false);
      return;
    }
    if (t.closest('.sidebar a, .sidebar button')) {
      setOpen(false);
      return;
    }
  });
  // Close on Escape — handy for keyboard users + accidental opens.
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') setOpen(false);
  });
})();
