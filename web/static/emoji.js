// Emoji picker — one shared popover reused by every chat composer.
//
// Design: the picker is pure front-end UI state. It never talks to the
// server. Picking an emoji writes it into the target composer field at the
// caret and dispatches an `input` event, so Datastar's data-bind syncs the
// bound signal exactly like a keystroke (same bridge mention.js/translate.js
// use). The composer keeps full ownership of the body signal.
//
// A trigger button calls window.fcEmojiToggle(el). The button resolves its
// target field via its data-emoji-target selector (preferred) or, failing
// that, the nearest composer field by DOM proximity. One panel node is built
// lazily and appended to <body>, then reused for every composer on the page.

(function () {
  if (window.fcEmojiToggle) return; // guard against double-include (layouts)

  // Curated, dependency-free set. [glyph, keywords] — keywords power search.
  const CATS = [
    {
      name: "Smileys",
      icon: "😀",
      items: [
        ["😀", "grin happy smile"], ["😁", "beam grin happy"], ["😂", "joy laugh tears"],
        ["🤣", "rofl laugh rolling"], ["😊", "blush smile happy"], ["😇", "angel innocent"],
        ["🙂", "slight smile"], ["😉", "wink"], ["😍", "love heart eyes"],
        ["😘", "kiss blow"], ["😜", "tongue wink silly"], ["🤪", "zany goofy"],
        ["🤗", "hug"], ["🤔", "thinking hmm"], ["🤨", "raised eyebrow skeptical"],
        ["😐", "neutral meh"], ["😴", "sleep tired zzz"], ["😎", "cool sunglasses"],
        ["🥳", "party celebrate"], ["😏", "smirk"], ["😢", "cry sad tear"],
        ["😭", "sob cry bawl"], ["😤", "huff steam angry"], ["😡", "angry mad rage"],
        ["🤯", "mind blown shock"], ["😱", "scream shock fear"], ["😳", "flushed embarrassed"],
        ["🥺", "pleading puppy beg"], ["😬", "grimace awkward"], ["🙄", "eye roll"],
        ["😅", "sweat nervous laugh"], ["🤭", "giggle oops hand"], ["🤫", "shush quiet"],
        ["😋", "yum tasty"], ["🤤", "drool"], ["🥱", "yawn bored"],
      ],
    },
    {
      name: "Gestures",
      icon: "👍",
      items: [
        ["👍", "thumbs up yes good"], ["👎", "thumbs down no bad"], ["👏", "clap applause"],
        ["🙌", "raise hands praise"], ["🙏", "pray thanks please"], ["🤝", "handshake deal"],
        ["👌", "ok perfect"], ["✌️", "peace victory"], ["🤞", "fingers crossed luck"],
        ["🤙", "call shaka"], ["💪", "muscle strong flex"], ["👋", "wave hi bye hello"],
        ["🤚", "hand stop"], ["✋", "hand high five"], ["👉", "point right"],
        ["👈", "point left"], ["👆", "point up"], ["👇", "point down"],
        ["✍️", "write hand"], ["🫶", "heart hands love"], ["🤷", "shrug dunno"],
        ["🫡", "salute respect"], ["🤦", "facepalm"], ["🫰", "fingers heart"],
      ],
    },
    {
      name: "Hearts",
      icon: "❤️",
      items: [
        ["❤️", "heart love red"], ["🧡", "orange heart"], ["💛", "yellow heart"],
        ["💚", "green heart"], ["💙", "blue heart"], ["💜", "purple heart"],
        ["🖤", "black heart"], ["🤍", "white heart"], ["🤎", "brown heart"],
        ["💔", "broken heart"], ["❣️", "heart exclamation"], ["💕", "two hearts love"],
        ["💞", "revolving hearts"], ["💓", "beating heart"], ["💗", "growing heart"],
        ["💖", "sparkling heart"], ["💘", "cupid arrow heart"], ["💝", "gift heart"],
        ["💟", "heart decoration"], ["♥️", "heart suit"],
      ],
    },
    {
      name: "Animals",
      icon: "🐶",
      items: [
        ["🐶", "dog puppy"], ["🐱", "cat kitten"], ["🐭", "mouse"], ["🐹", "hamster"],
        ["🐰", "rabbit bunny"], ["🦊", "fox"], ["🐻", "bear"], ["🐼", "panda"],
        ["🐨", "koala"], ["🐯", "tiger"], ["🦁", "lion"], ["🐮", "cow"],
        ["🐷", "pig"], ["🐸", "frog"], ["🐵", "monkey"], ["🐔", "chicken"],
        ["🐧", "penguin"], ["🐦", "bird"], ["🦄", "unicorn"], ["🐝", "bee"],
        ["🦋", "butterfly"], ["🐢", "turtle"], ["🐙", "octopus"], ["🐬", "dolphin"],
        ["🐳", "whale"], ["🦖", "dino t-rex"], ["🌸", "blossom flower"], ["🌹", "rose flower"],
      ],
    },
    {
      name: "Food",
      icon: "🍕",
      items: [
        ["🍕", "pizza"], ["🍔", "burger"], ["🍟", "fries"], ["🌭", "hotdog"],
        ["🌮", "taco"], ["🍣", "sushi"], ["🍜", "ramen noodles"], ["🍝", "pasta"],
        ["🍦", "ice cream"], ["🍩", "donut"], ["🍪", "cookie"], ["🎂", "cake birthday"],
        ["🍫", "chocolate"], ["🍿", "popcorn"], ["🍎", "apple"], ["🍌", "banana"],
        ["🍓", "strawberry"], ["🍉", "watermelon"], ["🥑", "avocado"], ["☕", "coffee"],
        ["🍺", "beer"], ["🍻", "cheers beer"], ["🥂", "champagne toast"], ["🍷", "wine"],
        ["🧋", "boba bubble tea"], ["🥗", "salad"],
      ],
    },
    {
      name: "Activity",
      icon: "⚽",
      items: [
        ["⚽", "soccer football"], ["🏀", "basketball"], ["🏈", "football"], ["⚾", "baseball"],
        ["🎾", "tennis"], ["🏐", "volleyball"], ["🎱", "pool 8ball"], ["🏓", "ping pong"],
        ["🏸", "badminton"], ["🥊", "boxing"], ["🏆", "trophy win"], ["🥇", "gold medal first"],
        ["🎯", "target dart bullseye"], ["🎮", "game controller"], ["🎲", "dice"], ["🎸", "guitar"],
        ["🎹", "piano keyboard"], ["🎤", "mic sing"], ["🎧", "headphones music"], ["🎨", "art paint"],
        ["🎬", "movie clapper"], ["🚀", "rocket launch"], ["🎉", "party tada celebrate"], ["🎊", "confetti"],
      ],
    },
    {
      name: "Objects",
      icon: "💡",
      items: [
        ["💡", "idea bulb light"], ["🔥", "fire lit hot"], ["⭐", "star"], ["✨", "sparkles"],
        ["⚡", "zap lightning fast"], ["💥", "boom collision"], ["💯", "hundred perfect"], ["💢", "anger"],
        ["💦", "sweat splash"], ["💤", "sleep zzz"], ["🎁", "gift present"], ["📌", "pin"],
        ["📎", "paperclip attach"], ["🔒", "lock secure"], ["🔑", "key"], ["🛠️", "tools fix"],
        ["💻", "laptop computer"], ["📱", "phone mobile"], ["💰", "money bag"], ["📈", "chart up growth"],
        ["📉", "chart down loss"], ["⏰", "alarm clock time"], ["📷", "camera photo"], ["🔔", "bell notify"],
        ["🕐", "clock time"], ["💬", "speech chat bubble"],
      ],
    },
    {
      name: "Symbols",
      icon: "✅",
      items: [
        ["✅", "check yes done tick"], ["❌", "cross no wrong"], ["❓", "question"], ["❗", "exclamation important"],
        ["⚠️", "warning caution"], ["🚫", "no forbidden"], ["♻️", "recycle"], ["✔️", "check tick"],
        ["➕", "plus add"], ["➖", "minus"], ["➗", "divide"], ["💲", "dollar money"],
        ["🆗", "ok"], ["🆕", "new"], ["🔝", "top up"], ["🔄", "refresh loop"],
        ["▶️", "play"], ["⏸️", "pause"], ["⏹️", "stop"], ["🔴", "red circle"],
        ["🟢", "green circle"], ["🔵", "blue circle"], ["🟡", "yellow circle"], ["⚫", "black circle"],
        ["💠", "diamond"], ["🔗", "link chain"],
      ],
    },
  ];

  const RECENT_KEY = "fc_emoji_recent";
  const RECENT_MAX = 24;

  let panel, searchEl, tabsEl, gridEl, activeBtn, activeCat;

  function loadRecent() {
    try {
      const v = JSON.parse(localStorage.getItem(RECENT_KEY) || "[]");
      return Array.isArray(v) ? v.slice(0, RECENT_MAX) : [];
    } catch (e) {
      return [];
    }
  }

  function pushRecent(glyph) {
    let r = loadRecent().filter((g) => g !== glyph);
    r.unshift(glyph);
    r = r.slice(0, RECENT_MAX);
    try {
      localStorage.setItem(RECENT_KEY, JSON.stringify(r));
    } catch (e) {
      /* private mode — recents simply won't persist */
    }
  }

  // Resolve the editable field this button feeds. Prefer an explicit
  // selector (data-emoji-target); else walk up to the nearest composer and
  // grab its visible textarea / text input (hidden inputs are type=hidden,
  // so the selector skips them).
  function targetField(btn) {
    const sel = btn.dataset.emojiTarget;
    if (sel) {
      const el = document.querySelector(sel);
      if (el) return el;
    }
    const wrap = btn.closest(
      ".composer, .pm-composer, .thread-composer, .field, .card"
    );
    if (!wrap) return null;
    return wrap.querySelector('textarea, input[type="text"]');
  }

  function insert(field, glyph) {
    if (!field) return;
    field.focus();
    const start = typeof field.selectionStart === "number" ? field.selectionStart : field.value.length;
    const end = typeof field.selectionEnd === "number" ? field.selectionEnd : field.value.length;
    const before = field.value.slice(0, start);
    const after = field.value.slice(end);
    field.value = before + glyph + after;
    const caret = start + glyph.length;
    try {
      field.setSelectionRange(caret, caret);
    } catch (e) {
      /* some input types disallow setSelectionRange */
    }
    // Auto-grow textareas (chat/lobby/forum composers size on input).
    if (field.tagName === "TEXTAREA") {
      field.style.height = "auto";
      field.style.height = Math.min(field.scrollHeight, 200) + "px";
    }
    field.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function emojiButton(glyph) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "fc-emoji";
    b.textContent = glyph;
    b.setAttribute("aria-label", glyph);
    b.tabIndex = -1;
    b.addEventListener("click", (e) => {
      e.preventDefault();
      const field = activeBtn && targetField(activeBtn);
      insert(field, glyph);
      pushRecent(glyph);
    });
    return b;
  }

  function renderCat(catName) {
    activeCat = catName;
    gridEl.innerHTML = "";
    [...tabsEl.children].forEach((t) =>
      t.classList.toggle("active", t.dataset.cat === catName)
    );
    let items;
    if (catName === "Recent") {
      items = loadRecent().map((g) => [g, ""]);
      if (!items.length) {
        const empty = document.createElement("p");
        empty.className = "fc-emoji-empty";
        empty.textContent = "No recent emoji yet — pick some below.";
        gridEl.appendChild(empty);
        return;
      }
    } else {
      const c = CATS.find((c) => c.name === catName);
      items = c ? c.items : [];
    }
    const frag = document.createDocumentFragment();
    items.forEach(([g]) => frag.appendChild(emojiButton(g)));
    gridEl.appendChild(frag);
  }

  function renderSearch(q) {
    q = q.trim().toLowerCase();
    if (!q) {
      renderCat(activeCat && activeCat !== "search" ? activeCat : CATS[0].name);
      return;
    }
    [...tabsEl.children].forEach((t) => t.classList.remove("active"));
    gridEl.innerHTML = "";
    const seen = new Set();
    const frag = document.createDocumentFragment();
    CATS.forEach((c) =>
      c.items.forEach(([g, kw]) => {
        if (seen.has(g)) return;
        if (kw.includes(q)) {
          seen.add(g);
          frag.appendChild(emojiButton(g));
        }
      })
    );
    if (!seen.size) {
      const empty = document.createElement("p");
      empty.className = "fc-emoji-empty";
      empty.textContent = "No emoji match “" + q + "”.";
      gridEl.appendChild(empty);
      return;
    }
    gridEl.appendChild(frag);
  }

  function build() {
    panel = document.createElement("div");
    panel.className = "fc-emoji-panel";
    panel.setAttribute("role", "dialog");
    panel.setAttribute("aria-label", "Emoji picker");
    panel.hidden = true;

    searchEl = document.createElement("input");
    searchEl.type = "search";
    searchEl.className = "fc-emoji-search";
    searchEl.placeholder = "Search emoji…";
    searchEl.setAttribute("aria-label", "Search emoji");
    searchEl.addEventListener("input", () => renderSearch(searchEl.value));

    tabsEl = document.createElement("div");
    tabsEl.className = "fc-emoji-tabs";
    const tabDefs = [{ name: "Recent", icon: "🕘" }, ...CATS];
    tabDefs.forEach((c) => {
      const t = document.createElement("button");
      t.type = "button";
      t.className = "fc-emoji-tab";
      t.dataset.cat = c.name;
      t.textContent = c.icon;
      t.title = c.name;
      t.setAttribute("aria-label", c.name);
      t.addEventListener("click", (e) => {
        e.preventDefault();
        searchEl.value = "";
        renderCat(c.name);
      });
      tabsEl.appendChild(t);
    });

    gridEl = document.createElement("div");
    gridEl.className = "fc-emoji-grid";

    panel.appendChild(searchEl);
    panel.appendChild(tabsEl);
    panel.appendChild(gridEl);
    document.body.appendChild(panel);
  }

  function position() {
    if (!activeBtn) return;
    const r = activeBtn.getBoundingClientRect();
    const narrow = window.innerWidth <= 560;
    if (narrow) {
      // Bottom sheet on phones — full width, anchored to viewport bottom.
      panel.classList.add("sheet");
      panel.style.left = "0px";
      panel.style.right = "0px";
      panel.style.top = "auto";
      panel.style.bottom = "0px";
      return;
    }
    panel.classList.remove("sheet");
    panel.style.right = "auto";
    panel.style.bottom = "auto";
    const pw = panel.offsetWidth || 320;
    const ph = panel.offsetHeight || 360;
    let left = r.left;
    if (left + pw > window.innerWidth - 8) left = window.innerWidth - pw - 8;
    if (left < 8) left = 8;
    // Prefer opening upward (composers sit near the viewport bottom).
    let top = r.top - ph - 8;
    if (top < 8) top = r.bottom + 8;
    panel.style.left = left + "px";
    panel.style.top = top + "px";
  }

  function open(btn) {
    activeBtn = btn;
    if (!panel.dataset.built) {
      renderCat(loadRecent().length ? "Recent" : CATS[0].name);
      panel.dataset.built = "1";
    }
    panel.hidden = false;
    btn.setAttribute("aria-expanded", "true");
    position();
    // Don't autofocus search on touch (avoids forcing the keyboard up).
    if (!matchMedia("(pointer: coarse)").matches) searchEl.focus();
  }

  function close() {
    if (!panel || panel.hidden) return;
    panel.hidden = true;
    if (activeBtn) activeBtn.setAttribute("aria-expanded", "false");
    activeBtn = null;
  }

  window.fcEmojiToggle = function (btn) {
    if (!panel) build();
    if (!panel.hidden && activeBtn === btn) {
      close();
      return;
    }
    open(btn);
  };

  // Dismiss on outside click / Escape. Clicks inside the panel or on the
  // active trigger are ignored.
  document.addEventListener("click", (e) => {
    if (!panel || panel.hidden) return;
    if (panel.contains(e.target)) return;
    if (activeBtn && activeBtn.contains(e.target)) return;
    close();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") close();
  });
  window.addEventListener("resize", () => {
    if (panel && !panel.hidden) position();
  });
})();
