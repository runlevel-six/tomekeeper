// Keyboard navigation. The interface brief asks for it from day one, because this is a reading
// tool and reaching for a mouse to advance one article is the friction that makes
// a reader stop using it.
//
// Hand-written and small, for the same reason the CSS is: there is no build step,
// nothing here needs a framework, and a page that works without JavaScript should
// not be made to depend on it. Everything below is an accelerator for something
// that is already a link or a form.
//
//   j / ↓   next entry
//   k / ↑   previous entry
//   o / ⏎   open the selected entry
//   n       next article (in the reader)
//   p       previous article (in the reader)
//   u / esc back to the list the article was opened from
//   r       reload this page
//   s       star or unstar
//   m       mark read or unread
//   /       jump to search
//   g then u / a / s / f / c   go to unread, everything, starred, feeds, categories
(function () {
  "use strict";

  var selected = -1;

  // Mirrors the unread count onto the installed app's icon.
  //
  // This is the only thing here that is not an accelerator for a control already
  // on the page — there is no way to draw on an app icon in markup. Guarded
  // because setAppBadge exists on some platforms and not others, and clearing at
  // zero matters as much as setting: a badge that stays at 12 after everything is
  // read is worse than no badge, because it is a lie the reader cannot dismiss.
  function badge() {
    if (!navigator.setAppBadge) return;

    var count = parseInt(document.body.getAttribute("data-unread") || "0", 10);
    if (isNaN(count) || count <= 0) {
      if (navigator.clearAppBadge) navigator.clearAppBadge().catch(function () {});
      return;
    }
    navigator.setAppBadge(count).catch(function () {});
  }

  badge();

  // Follows a link the page has already drawn, so a keyboard shortcut can never
  // navigate somewhere the reader had no visible way to reach.
  function follow(selector) {
    var link = document.querySelector(selector);
    if (link) {
      link.click();
      return true;
    }
    return false;
  }

  function entries() {
    return Array.prototype.slice.call(document.querySelectorAll(".entry"));
  }

  function select(index) {
    var all = entries();
    if (all.length === 0) return;

    // Clamp rather than wrap. Wrapping from the last entry to the first is
    // disorienting when you cannot see both ends of the list.
    if (index < 0) index = 0;
    if (index >= all.length) index = all.length - 1;

    all.forEach(function (el) {
      el.classList.remove("is-selected");
    });

    var el = all[index];
    el.classList.add("is-selected");
    selected = index;

    // Keep the selection visible without yanking the page around when it already
    // is: nearest scrolls only as far as it must.
    el.scrollIntoView({ block: "nearest" });
  }

  function current() {
    var all = entries();
    if (selected < 0 || selected >= all.length) return null;
    return all[selected];
  }

  function open() {
    var el = current();
    if (!el) return;
    var link = el.querySelector("h2 a, h1 a");
    if (link) link.click();
  }

  // Clicks the toggle inside the selected entry, or the one on an article page.
  // Going through the button rather than issuing a request means htmx swaps the
  // control exactly as it would on a click, and the page needs no separate code
  // path for keyboard-driven changes.
  function press(which) {
    var scope = current() || document.querySelector(".reader");
    if (!scope) return;

    var forms = scope.querySelectorAll("form[action*='/" + which + "']");
    if (forms.length === 0) return;

    var button = forms[0].querySelector("button");
    if (button) button.click();
  }

  // Marking articles read as they are scrolled past.
  //
  // Inert unless the reader asked for it: the list carries data-mark-on-scroll only
  // when their preference and the list itself both allow it, which the server has
  // already resolved. Three properties, in the order they matter.
  //
  // **A row has to have been looked at.** It must be on screen for the best part of
  // a second before leaving upwards counts. A flick from the top of a page to the
  // bottom therefore marks nothing, and it does so for a structural reason rather
  // than a hopeful one: IntersectionObserver coalesces a fast scroll into a single
  // report per row, so the rows flung past never accumulate any visible time.
  //
  // **Leaving upwards, not merely leaving.** Scrolling back down past something you
  // had scrolled up to re-read must not mark it, so the exit is checked against the
  // top edge of the viewport.
  //
  // **One request for many rows.** A reader clearing a morning's backlog would
  // otherwise open fifty connections, one per row, each waiting on the last. These
  // accumulate and go in batches, with a beacon on the way out so that the last few
  // are not lost to a closed tab.
  //
  // What may actually be marked is the server's decision — starred and saved
  // articles are refused there, and the preference is re-read on every request — so
  // nothing here needs to know those rules or be trusted to apply them.
  var scrollMark = (function () {
    var list = document.querySelector(".stream[data-mark-on-scroll]");
    if (!list || !window.IntersectionObserver) return null;

    // Long enough that a row scrolled past on the way somewhere else is not
    // "read", short enough that a reader skimming headlines is not fighting it.
    var dwellMs = 900;
    // A pause in scrolling means the reader has stopped, which is the moment to
    // report what they have finished with.
    var quietMs = 1200;
    // Or sooner, if enough rows have gone by that waiting risks losing them.
    var flushAt = 25;
    // The server refuses more than 200 in one request.
    var maxBatch = 200;

    var seen = new Map();      // element -> when it first became visible
    var claimed = new Set();   // ids this page has already sent or queued
    var pending = [];
    var timer = null;
    var on = true;

    var observer = new IntersectionObserver(function (records) {
      records.forEach(function (record) {
        if (record.isIntersecting) {
          if (!seen.has(record.target)) seen.set(record.target, performance.now());
          return;
        }

        var since = seen.get(record.target);
        if (since === undefined) return;                       // never on screen
        if (performance.now() - since < dwellMs) return;        // glanced past
        // boundingClientRect is the geometry at the moment of the report, so this
        // asks the question that matters: did it leave over the top?
        if (record.boundingClientRect.bottom > 0) return;       // left downwards

        passed(record.target);
      });
    });

    // passed reports one row as finished with. Also called by the keyboard, where
    // moving the selection down past an entry means the same thing.
    function passed(entry) {
      if (!on || !entry) return;

      var id = entry.getAttribute("data-article");
      if (!id || claimed.has(id)) return;
      // A row already read needs nothing, and asking would spend a request to be
      // told so.
      if (entry.classList.contains("is-read")) return;

      claimed.add(id);
      pending.push(id);
      observer.unobserve(entry);

      // Dimmed here rather than waiting for the response, because the reader is
      // still scrolling and the row is the feedback. Only the class: the controls
      // inside the row are redrawn by the server's own fragment, so the button
      // cannot end up saying something the database disagrees with.
      entry.classList.add("is-read");

      if (pending.length >= flushAt) {
        send(false);
        return;
      }
      if (timer) window.clearTimeout(timer);
      timer = window.setTimeout(function () { send(false); }, quietMs);
    }

    // send posts a batch. leaving uses a beacon, which survives the page going
    // away but cannot bring back the redrawn controls — there is no page left to
    // put them on.
    function send(leaving) {
      if (timer) {
        window.clearTimeout(timer);
        timer = null;
      }
      if (!on || pending.length === 0) return;

      var batch = pending.splice(0, maxBatch);
      var ids = batch.join(",");

      if (leaving && navigator.sendBeacon) {
        navigator.sendBeacon("/mark-read/scrolled", new URLSearchParams({ ids: ids }));
        return;
      }

      // Through htmx so the response's out-of-band fragments are applied: each one
      // is the same control the row would have rendered on a reload, which is why
      // this does not redraw any buttons itself.
      if (window.htmx) {
        window.htmx
          .ajax("POST", "/mark-read/scrolled", { source: list, swap: "none", values: { ids: ids } })
          .catch(stop);
        return;
      }

      fetch("/mark-read/scrolled", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({ ids: ids }),
        keepalive: true
      }).then(function (response) {
        if (!response.ok) stop();
      }).catch(stop);
    }

    // stop gives up for the life of this page. The expected cause is the reader
    // turning the preference off in another tab, which the server answers with a
    // refusal; retrying that on every scroll would be a request per row forever.
    function stop() {
      on = false;
      pending.length = 0;
      observer.disconnect();
    }

    // watch picks up rows, including the ones infinite scroll appends later.
    function watch() {
      if (!on) return;
      entries().forEach(function (entry) {
        if (entry.hasAttribute("data-watched")) return;
        entry.setAttribute("data-watched", "");
        if (entry.classList.contains("is-read")) return;
        observer.observe(entry);
      });
    }

    watch();
    document.body.addEventListener("htmx:afterSettle", watch);

    // Both events, because neither is reliable alone: pagehide is what fires on a
    // navigation, and visibilitychange is what fires when a phone's browser is
    // backgrounded and may never come back.
    window.addEventListener("pagehide", function () { send(true); });
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "hidden") send(true);
    });

    return { passed: passed };
  })();

  function typing(el) {
    if (!el) return false;
    var tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
  }

  var awaitingGo = false;

  document.addEventListener("keydown", function (event) {
    // Never steal a key from a text field, and never from a shortcut the browser
    // or the operating system owns.
    if (typing(event.target)) return;
    if (event.ctrlKey || event.metaKey || event.altKey) return;

    if (awaitingGo) {
      awaitingGo = false;
      var destinations = {
        u: "/", a: "/all", s: "/starred", f: "/feeds", c: "/categories"
      };
      var to = destinations[event.key];
      if (to) {
        event.preventDefault();
        window.location.href = to;
      }
      return;
    }

    switch (event.key) {
      case "j":
      case "ArrowDown":
        event.preventDefault();
        // The entry being left behind, reported only if the selection actually
        // moved: at the end of the list j is clamped, and pressing it against the
        // bottom must not keep marking the row it cannot leave.
        var leaving = current();
        select(selected + 1);
        if (scrollMark && leaving && current() !== leaving) scrollMark.passed(leaving);
        break;
      case "k":
      case "ArrowUp":
        event.preventDefault();
        select(selected - 1);
        break;
      case "o":
      case "Enter":
        event.preventDefault();
        open();
        break;
      case "s":
        event.preventDefault();
        press("star");
        break;
      case "m":
        event.preventDefault();
        press("read");
        break;

      // Moving along the list from inside an article, and getting back out of
      // one. These are the keys that matter most in the reader, because it is
      // the one view with nowhere else to go.
      case "n":
        if (follow(".reader-nav a[rel='next']")) event.preventDefault();
        break;
      case "p":
        if (follow(".reader-nav a[rel='prev']")) event.preventDefault();
        break;
      case "u":
      case "Escape":
        if (follow(".reader-nav a[rel='up']")) event.preventDefault();
        break;

      // Reload, because an installed app has no reload button. The control is on
      // the page, and this presses it.
      case "r":
        if (follow(".chrome .reload")) event.preventDefault();
        break;

      case "g":
        awaitingGo = true;
        break;
      case "/":
        event.preventDefault();
        window.location.href = "/search";
        break;
      default:
        break;
    }
  });

  // A click moves the selection there, so switching between mouse and keyboard
  // mid-list does not send the next j back to the top.
  document.addEventListener("click", function (event) {
    var entry = event.target.closest ? event.target.closest(".entry") : null;
    if (!entry) return;
    var index = entries().indexOf(entry);
    if (index >= 0) select(index);
  });
})();
