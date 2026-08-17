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
//   s       star or unstar
//   m       mark read or unread
//   /       jump to search
//   g then u / a / s / f   go to unread, everything, starred, feeds
(function () {
  "use strict";

  var selected = -1;

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
      var destinations = { u: "/", a: "/all", s: "/starred", f: "/feeds" };
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
        select(selected + 1);
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
