// Annotation marks inside a document frame. The frame is sandboxed into an opaque origin and holds
// no room credential: it draws what the room page sends and proposes anchors; the room page checks
// every message and does the saving.
//
// The text model, the quote anchoring and the selection capture are ported from
// github.com/babs/grip-live-diff defaults/static/js/annotate.js at commit 36209e6.
// internal/anchor runs the same rules on the server: change both together.
(function () {
  'use strict';
  var CONTEXT = 32, MAX_EXACT = 4000, FINGERPRINT = 80;
  // Text that is not the document: diagrams and formulas (their rendered text is not in the
  // source), scripts, buttons, hidden parts, diff deletions, this script's own UI.
  var SKIP = 'script, style, noscript, template, button, mjx-container, del.gg-del, [hidden], .mermaid, .math, .gr-annot-ui';
  var STEP = /([a-zA-Z][a-zA-Z0-9-]*):nth-of-type\((\d+)\)/g;
  // The room page is served by the same server as this document.
  var room = new URL(location.href).origin;
  var supported = 'highlights' in CSS && typeof Highlight === 'function';

  var article, model, items = [], entries = [], canCreate = false, picking = false, hovered = null, active = null, pending = null;
  var bubble, timer;

  function send(msg) {
    parent.postMessage(msg, room);
  }

  // skippedWithin tells whether node sits in skipped markup below root (root itself not counted).
  function skippedWithin(node, root) {
    for (var e = node.nodeType === 1 ? node : node.parentElement; e && e !== root; e = e.parentElement) {
      if (e.matches(SKIP)) return true;
    }
    return false;
  }

  // fold collapses whitespace runs to one space; map[i] is the raw index of folded char i.
  // Anchors are compared folded, so a re-wrapped paragraph still matches its quote.
  function fold(raw) {
    var out = '', map = [], space = false;
    for (var i = 0; i < raw.length; i++) {
      var c = raw[i];
      if (/\s/.test(c)) {
        if (!space && out.length) {
          out += ' ';
          map.push(i);
        }
        space = true;
        continue;
      }
      space = false;
      out += c;
      map.push(i);
    }
    return { text: out, map: map };
  }

  // The text model: every text node of the article outside SKIP, with its raw offset.
  function buildModel() {
    var segs = [], raw = '', walker = document.createTreeWalker(article, NodeFilter.SHOW_TEXT);
    for (var node = walker.nextNode(); node; node = walker.nextNode()) {
      if (skippedWithin(node, article)) continue;
      segs.push({ node: node, start: raw.length });
      raw += node.data;
    }
    var folded = fold(raw);
    return { segs: segs, text: folded.text, map: folded.map };
  }

  // Raw offset of a DOM point, counting only modelled text.
  function offsetOf(container, offset) {
    var probe = document.createRange();
    probe.setStart(container, offset);
    probe.collapse(true);
    var total = 0;
    for (var i = 0; i < model.segs.length; i++) {
      var seg = model.segs[i];
      if (probe.comparePoint(seg.node, seg.node.length) <= 0) {
        total += seg.node.length;
        continue;
      }
      if (probe.comparePoint(seg.node, 0) >= 0) break;
      total += seg.node === container ? offset : 0;
      break;
    }
    return total;
  }

  function foldedOffset(raw) {
    var lo = 0, hi = model.map.length;
    while (lo < hi) {
      var mid = (lo + hi) >> 1;
      if (model.map[mid] < raw) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  // Folded [start, end) to live Ranges, one per text node, so skipped text in between stays unmarked.
  function rangesOf(start, end) {
    var rawStart = model.map[start], rawEnd = model.map[end - 1] + 1, out = [];
    for (var i = 0; i < model.segs.length; i++) {
      var seg = model.segs[i], segEnd = seg.start + seg.node.length;
      if (segEnd <= rawStart || seg.start >= rawEnd) continue;
      var range = document.createRange();
      range.setStart(seg.node, Math.max(rawStart, seg.start) - seg.start);
      range.setEnd(seg.node, Math.min(rawEnd, segEnd) - seg.start);
      if (!range.collapsed) out.push(range);
    }
    return out;
  }

  // How many characters of prefix end at idx, plus how many of suffix start at idx + length.
  function context(text, idx, length, prefix, suffix) {
    var n = 0, m = 0;
    while (n < prefix.length && idx - 1 - n >= 0 && text[idx - 1 - n] === prefix[prefix.length - 1 - n]) n++;
    while (m < suffix.length && text[idx + length + m] === suffix[m]) m++;
    return n + m;
  }

  // Anchor a quote in the folded text: exact hits first (prefix/suffix pick among several), then
  // the Hypothesis fallback between prefix and suffix, an empty one standing for the document's
  // start or end; null when the text is gone.
  function anchorText(a) {
    var text = model.text, exact = a.exact, prefix = a.prefix || '', suffix = a.suffix || '';
    var best = -1, score = -1;
    for (var idx = text.indexOf(exact); idx >= 0; idx = text.indexOf(exact, idx + 1)) {
      var s = context(text, idx, exact.length, prefix, suffix);
      if (s > score) {
        score = s;
        best = idx;
      }
    }
    if (best >= 0) return { start: best, end: best + exact.length };
    var from = 0, to = text.length;
    if (prefix) {
      var p = text.indexOf(prefix);
      if (p < 0) return null;
      from = p + prefix.length;
    }
    if (suffix) {
      var q = text.indexOf(suffix, from);
      if (q < 0) return null;
      to = q;
    }
    if (to <= from || Array.from(text.slice(from, to)).length > 3 * Array.from(exact).length) return null;
    while (text[from] === ' ') from++;
    while (to > from && text[to - 1] === ' ') to--;
    return to > from ? { start: from, end: to } : null;
  }

  function cut(s) {
    return Array.from(s).slice(0, FINGERPRINT).join('');
  }

  // An element's folded text cut to 80 characters; for one with no text (an image), its first
  // non-empty alt, aria-label, title or src, marked with "@".
  function fingerprint(el) {
    var raw = '', walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    for (var n = walker.nextNode(); n; n = walker.nextNode()) {
      if (!skippedWithin(n, el)) raw += n.data;
    }
    var t = fold(raw).text.trim();
    if (t) return cut(t);
    var keys = ['alt', 'aria-label', 'title', 'src'];
    for (var i = 0; i < keys.length; i++) {
      var v = (el.getAttribute(keys[i]) || '').trim();
      if (v) return '@' + cut(fold(v).text);
    }
    return '';
  }

  function pathOf(el) {
    var steps = [];
    for (; el && el !== document.body; el = el.parentElement) {
      var k = 1;
      for (var s = el.previousElementSibling; s; s = s.previousElementSibling) if (s.localName === el.localName) k++;
      steps.unshift(el.localName + ':nth-of-type(' + k + ')');
    }
    return ['body'].concat(steps).join(' > ');
  }

  // walk follows the path step by step: no selector engine reads it, as on the server.
  function walk(path) {
    var el = document.body, m;
    STEP.lastIndex = 0;
    while ((m = STEP.exec(path))) {
      var k = +m[2], next = null;
      for (var c = el.firstElementChild; c; c = c.nextElementSibling) {
        if (c.localName === m[1] && --k === 0) {
          next = c;
          break;
        }
      }
      if (!next) return null;
      el = next;
    }
    return el;
  }

  // An element at its path when it still has its tag and fingerprint, else the first one of that
  // tag with that fingerprint: a re-render that moves or rebuilds it keeps the mark.
  function resolveElement(a) {
    var el = walk(a.path);
    if (el && el.localName === a.tag && fingerprint(el) === a.fingerprint) return el;
    if (!a.fingerprint) return null;
    var all = document.body.getElementsByTagName(a.tag);
    for (var i = 0; i < all.length; i++) {
      if (all[i].localName === a.tag && fingerprint(all[i]) === a.fingerprint) return all[i];
    }
    return null;
  }

  function draw() {
    model = buildModel();
    document.querySelectorAll('[data-gr-annot]').forEach(function (el) {
      el.removeAttribute('data-gr-annot');
      el.removeAttribute('data-gr-done');
      el.removeAttribute('data-gr-active');
    });
    var groups = { 'gr-annot': [], 'gr-annot-done': [], 'gr-annot-active': [] };
    entries = [];
    items.forEach(function (it) {
      var a = it.anchor;
      if (!a) return;
      var entry = { id: it.id, ranges: [], el: null };
      if (a.kind === 'text') {
        var at = anchorText(a);
        if (at) entry.ranges = rangesOf(at.start, at.end);
        var group = it.status === 'done' ? 'gr-annot-done' : 'gr-annot';
        groups[group].push.apply(groups[group], entry.ranges);
        if (active === it.id) groups['gr-annot-active'].push.apply(groups['gr-annot-active'], entry.ranges);
      } else {
        entry.el = resolveElement(a);
        if (entry.el) {
          var ids = entry.el.getAttribute('data-gr-annot');
          entry.el.setAttribute('data-gr-annot', ids ? ids + ' ' + it.id : String(it.id));
          if (it.status === 'done') entry.el.setAttribute('data-gr-done', '');
          if (active === it.id) entry.el.setAttribute('data-gr-active', '');
        }
      }
      entries.push(entry);
    });
    if (supported) {
      Object.keys(groups).forEach(function (name) {
        CSS.highlights.set(name, new Highlight(...groups[name]));
      });
    }
  }

  // Redraw after the document changes under the marks: a script re-render, a diagram drawn late.
  // Attributes are not watched, so the marks' own attributes never loop back here.
  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(draw, 50);
  }

  // The selection as a quote, or null when it is not usable (empty, outside the article, starting
  // or ending in skipped text, too long).
  function quoteOf(selection) {
    if (!selection || selection.isCollapsed || !selection.rangeCount) return null;
    var range = selection.getRangeAt(0);
    var start = range.startContainer, end = range.endContainer;
    if (!article.contains(start) || !article.contains(end)) return null;
    if (skippedWithin(start, article) || skippedWithin(end, article)) return null;
    var from = foldedOffset(offsetOf(start, range.startOffset));
    var to = foldedOffset(offsetOf(end, range.endOffset));
    var text = model.text.slice(from, to);
    var lead = text.length - text.replace(/^\s+/, '').length;
    from += lead;
    text = text.trim();
    if (!text || text.length > MAX_EXACT) return null;
    to = from + text.length;
    return {
      anchor: {
        kind: 'text',
        exact: text,
        prefix: model.text.slice(Math.max(0, from - CONTEXT), from),
        suffix: model.text.slice(to, to + CONTEXT),
      },
      rect: range.getBoundingClientRect(),
    };
  }

  function showBubble() {
    var quote = canCreate && !picking ? quoteOf(document.getSelection()) : null;
    pending = quote && quote.anchor;
    bubble.hidden = !quote;
    if (!quote) return;
    bubble.style.left = Math.max(4, Math.min(quote.rect.right, window.innerWidth - bubble.offsetWidth - 8)) + 'px';
    bubble.style.top = Math.min(quote.rect.bottom + 6, window.innerHeight - bubble.offsetHeight - 4) + 'px';
  }

  // Highlights are not elements: the entry under the pointer is found by hit-testing the caret
  // position against every anchored range.
  function entryAt(x, y) {
    var pos = document.caretPositionFromPoint ? document.caretPositionFromPoint(x, y) : null;
    var node = pos ? pos.offsetNode : null, offset = pos ? pos.offset : 0;
    if (!pos && document.caretRangeFromPoint) {
      var r = document.caretRangeFromPoint(x, y);
      if (r) {
        node = r.startContainer;
        offset = r.startOffset;
      }
    }
    if (!node) return null;
    for (var i = 0; i < entries.length; i++) {
      for (var j = 0; j < entries[i].ranges.length; j++) {
        if (entries[i].ranges[j].isPointInRange(node, offset)) return entries[i];
      }
    }
    return null;
  }

  // The element a pick lands on: skipped markup (a diagram, a formula) is picked whole, since what
  // is inside it is redrawn at will.
  function pickTarget(t) {
    if (!(t instanceof Element) || !document.body.contains(t) || t.closest('.gr-annot-ui')) return null;
    var whole = t.closest(SKIP);
    return whole && document.body.contains(whole) ? whole : t;
  }

  function hover(el) {
    if (hovered) hovered.removeAttribute('data-gr-pick');
    hovered = el;
    if (el) el.setAttribute('data-gr-pick', '');
  }

  function setPicking(on) {
    picking = on;
    document.documentElement.toggleAttribute('data-gr-picking', on);
    if (!on) hover(null);
    showBubble();
  }

  function focus(id) {
    active = id;
    draw();
    for (var i = 0; i < entries.length; i++) {
      var e = entries[i];
      if (e.id !== id) continue;
      var target = e.el || (e.ranges[0] && e.ranges[0].startContainer.parentElement);
      if (target) target.scrollIntoView({ block: 'center' });
    }
  }

  function receive(ev) {
    // Only the room page talks to this frame; the document's own scripts post from this window.
    if (ev.source !== parent || !ev.data || typeof ev.data !== 'object') return;
    var msg = ev.data;
    if (msg.type === 'gr:annotations') {
      items = Array.isArray(msg.items) ? msg.items : [];
      canCreate = msg.canCreate === true;
      if (!canCreate) setPicking(false);
      draw();
      showBubble();
    } else if (msg.type === 'gr:focus') {
      focus(msg.id);
    } else if (msg.type === 'gr:pick') {
      setPicking(msg.on === true && canCreate);
    }
  }

  function mount() {
    bubble = document.createElement('button');
    bubble.type = 'button';
    bubble.className = 'gr-annot-ui gr-annot-bubble';
    bubble.textContent = 'Annotate';
    bubble.hidden = true;
    // mousedown, not click: a click first collapses the selection the bubble is about.
    bubble.addEventListener('mousedown', function (ev) {
      ev.preventDefault();
      if (!pending) return;
      send({ type: 'gr:anchor', anchor: pending });
      bubble.hidden = true;
    });
    // Outside body: the bubble is no part of the document, its text model or its paths.
    document.documentElement.append(bubble);

    document.addEventListener('selectionchange', showBubble);
    document.addEventListener('pointermove', function (ev) {
      if (picking) hover(pickTarget(ev.target));
    });
    // Capture phase: in pick mode the document's own handlers must not see the click.
    document.addEventListener('click', function (ev) {
      if (picking) {
        ev.preventDefault();
        ev.stopPropagation();
        var el = pickTarget(ev.target);
        if (!el) return;
        setPicking(false);
        send({ type: 'gr:anchor', anchor: { kind: 'element', path: pathOf(el), tag: el.localName, fingerprint: fingerprint(el) } });
        return;
      }
      // A drag-select ending on a mark also fires click: that is a selection, not a click.
      if (ev.detail >= 2 || !document.getSelection().isCollapsed) return;
      var marked = ev.target instanceof Element && ev.target.closest('[data-gr-annot]');
      if (marked) {
        send({ type: 'gr:open', id: +marked.getAttribute('data-gr-annot').split(' ')[0] });
        return;
      }
      var e = entryAt(ev.clientX, ev.clientY);
      if (e) send({ type: 'gr:open', id: e.id });
    }, true);
    document.addEventListener('keydown', function (ev) {
      if (ev.key === 'Escape' && picking) {
        setPicking(false);
        send({ type: 'gr:pick', on: false });
      }
    });
    window.addEventListener('message', receive);
    new MutationObserver(schedule).observe(article, { childList: true, subtree: true, characterData: true });
  }

  document.addEventListener('DOMContentLoaded', function () {
    article = document.querySelector('.container-inner') || document.body;
    if (!article) return;
    mount();
    model = buildModel();
    send({ type: 'gr:ready' });
  });
})();
