'use strict';
// Shared by every page (docs/design.md, 005-AC39): the confirm sheet that replaces the browser's
// confirm dialog, the coarse "2 min ago", the date format, the icon set and the styled tip. A page
// loads it as a classic script; its modules call window.grillConfirm, window.grillAgo and the rest.

{
  /** @type {Promise<boolean> | null} */
  let open = null;

  /**
   * grillConfirm asks a question in a sheet: a title, one line on the consequence, Cancel and a
   * danger button. It resolves true once the danger button is pressed, false on Cancel, Escape or
   * a click outside. While one is open, another call answers false at once: a double click acts once.
   * @param {string} title
   * @param {string} text
   * @param {string} action
   * @returns {Promise<boolean>}
   */
  window.grillConfirm = (title, text, action) => {
    if (open) return open.then(() => false);
    open = new Promise((resolve) => {
      const d = document.createElement('dialog');
      d.className = 'sheet confirm';
      d.setAttribute('aria-labelledby', 'confirm-title');
      d.setAttribute('aria-describedby', 'confirm-text');
      const h = document.createElement('h2');
      h.id = 'confirm-title';
      h.textContent = title;
      const p = document.createElement('p');
      p.id = 'confirm-text';
      p.textContent = text;
      const actions = document.createElement('div');
      actions.className = 'actions';
      const cancel = document.createElement('button');
      cancel.type = 'button';
      cancel.className = 'btn-secondary';
      cancel.textContent = 'Cancel';
      const ok = document.createElement('button');
      ok.type = 'button';
      ok.className = 'btn-danger';
      ok.textContent = action;
      actions.append(cancel, ok);
      d.append(h, p, actions);
      let answer = false;
      cancel.addEventListener('click', () => d.close());
      ok.addEventListener('click', () => {
        answer = true;
        d.close();
      });
      // A press that starts and ends outside the sheet closes it, as every sheet does; one that
      // starts inside, a text selection say, does not.
      const outside = (/** @type {MouseEvent} */ e) => {
        const r = d.getBoundingClientRect();
        return e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom;
      };
      let pressedOutside = false;
      d.addEventListener('pointerdown', (e) => { pressedOutside = e.target === d && outside(e); });
      d.addEventListener('click', (e) => {
        if (pressedOutside && e.target === d && outside(e)) d.close();
        pressedOutside = false;
      });
      d.addEventListener('close', () => {
        d.remove();
        open = null;
        resolve(answer);
      });
      document.body.append(d);
      d.showModal();
      cancel.focus();
    });
    return open;
  };

  /**
   * grillAgo says how long ago t was, coarsely: "just now", "5 min ago", "3 h ago", "2 d ago".
   * @param {string} t
   * @returns {string}
   */
  window.grillAgo = (t) => {
    const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  };

  /**
   * grillDate is the one way a page writes a date: "Oct 14, 2026, 8:12 PM" in the reader's locale.
   * @param {string | number | Date} t
   * @returns {string}
   */
  window.grillDate = (t) => new Date(t).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });

  /**
   * grillTime is a time of today's lines: "08:12 PM".
   * @param {string | number | Date} t
   * @returns {string}
   */
  window.grillTime = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

  // The icon set (docs/design.md §5): 24-unit strokes in currentColor, drawn at text size. No
  // emoji or arrow glyph stands for a control.
  /** @type {Record<string, string>} */
  const icons = {
    close: '<path d="M18 6 6 18M6 6l12 12"/>',
    more: '<circle cx="5" cy="12" r="1.5" fill="currentColor"/><circle cx="12" cy="12" r="1.5" fill="currentColor"/><circle cx="19" cy="12" r="1.5" fill="currentColor"/>',
    send: '<path d="m22 2-7 20-4-9-9-4z"/><path d="M22 2 11 13"/>',
    width: '<path d="m18 8 4 4-4 4M6 8l-4 4 4 4M2 12h20"/>',
    fold: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M15 3v18M8 9l3 3-3 3"/>',
    chat: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
    docs: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6M16 13H8M16 17H8M10 9H8"/>',
    people: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
    back: '<path d="M19 12H5M12 19l-7-7 7-7"/>',
    mic: '<rect x="9" y="2" width="6" height="12" rx="3"/><path d="M19 10v1a7 7 0 0 1-14 0v-1M12 18v4"/>',
    'mic-off': '<path d="m2 2 20 20M15 9.3V5a3 3 0 0 0-5.7-1.3M9 9v2a3 3 0 0 0 5.1 2.1M19 10v1a7 7 0 0 1-1.1 3.8M5 10v1a7 7 0 0 0 11.6 5.3M12 18v4"/>',
    down: '<path d="m6 9 6 6 6-6"/>',
    clip: '<path d="m21.4 11.1-9.2 9.2a6 6 0 0 1-8.5-8.5l8.6-8.6a4 4 0 0 1 5.7 5.7l-8.6 8.6a2 2 0 0 1-2.8-2.8l8.5-8.5"/>',
    lock: '<rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
    eye: '<path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    alert: '<circle cx="12" cy="12" r="10"/><path d="M12 8v4M12 16h.01"/>',
    external: '<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    invite: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M19 8v6M22 11h-6"/>',
    settings: '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>',
    personas: '<path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/><path d="M19 15l.8 2.2L22 18l-2.2.8L19 21l-.8-2.2L16 18l2.2-.8z"/>',
    host: '<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M8 21h8M12 17v4"/>',
    rename: '<path d="M12 20h9"/><path d="M16.4 3.6a2.1 2.1 0 1 1 3 3L7 19l-4 1 1-4z"/>',
    bell: '<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9M10.3 21a1.9 1.9 0 0 0 3.4 0"/>',
    device: '<rect x="5" y="2" width="14" height="20" rx="2"/><path d="M12 18h.01"/>',
    profile: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
    download: '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/>',
    trash: '<path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>',
  };

  /**
   * grillIconHTML is the markup of one icon, for a template: aria-hidden, the control names itself.
   * @param {string} name
   * @returns {string}
   */
  window.grillIconHTML = (name) => '<svg class="ico" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"'
    + ' stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + icons[name] + '</svg>';

  /**
   * grillIcon is one icon as an element.
   * @param {string} name
   * @returns {Element}
   */
  window.grillIcon = (name) => {
    const t = document.createElement('template');
    t.innerHTML = window.grillIconHTML(name);
    return /** @type {Element} */ (t.content.firstChild);
  };
  // Markup names its icons with data-icon; they are drawn once, ahead of the label.
  for (const el of document.querySelectorAll('[data-icon]')) el.prepend(window.grillIcon(/** @type {string} */ (el.getAttribute('data-icon'))));

  /**
   * grillPlace puts popover pop below anchor, or above it when the bottom edge would cut it, left
   * of the row's ⋯ so it never covers one, shifted left when the right edge would cut it.
   * @param {HTMLElement} pop
   * @param {Element} anchor
   */
  window.grillPlace = (pop, anchor) => {
    const r = anchor.getBoundingClientRect();
    const more = /** @type {HTMLElement | null | undefined} */ (anchor.closest('li')?.querySelector('.person-more'));
    const right = more && !more.hidden ? more.getBoundingClientRect().left - 4 : innerWidth - 8;
    pop.style.maxWidth = Math.min(20 * parseFloat(getComputedStyle(document.documentElement).fontSize), Math.max(160, right - 8)) + 'px';
    pop.style.left = Math.max(8, Math.min(r.left, right - pop.offsetWidth)) + 'px';
    pop.style.top = Math.max(8, r.bottom + 4 + pop.offsetHeight > innerHeight - 8 ? r.top - 4 - pop.offsetHeight : r.bottom + 4) + 'px';
  };

  // The styled tip for every [data-tip] (docs/design.md §5), never a native title: after 300 ms of
  // hover, at once on keyboard focus. The pointer may move onto it and stay (WCAG 1.4.13).
  const tip = document.getElementById('person-tip') || Object.assign(document.createElement('div'), { id: 'person-tip', className: 'tip' });
  if (!tip.isConnected) {
    tip.popover = 'manual';
    tip.setAttribute('aria-hidden', 'true');
    document.body.append(tip);
  }
  /** @type {Element | null} */
  let tipFor = null;
  /** @type {number | undefined} */
  let tipHover;
  /** @type {number | undefined} */
  let tipHide;
  /** @param {EventTarget | null} t */
  const tipped = (t) => (t instanceof Element ? t.closest('[data-tip]:not([data-tip=""])') : null);
  // A details card open on a person says more than any tip.
  const cardOpen = () => !!document.querySelector('.person-card:popover-open');
  /** @param {Element} el */
  const showTip = (el) => {
    clearTimeout(tipHide);
    tipFor = el;
    tip.textContent = /** @type {HTMLElement} */ (el).dataset.tip || '';
    // A control's tip lets the pointer through: shown under a row, it would take the next row's click.
    tip.classList.toggle('pass', el.matches('button, a, [role=button]'));
    if (!tip.matches(':popover-open')) tip.showPopover();
    window.grillPlace(tip, el);
  };
  const hideTip = () => {
    clearTimeout(tipHover);
    clearTimeout(tipHide);
    tipFor = null;
    if (tip.matches(':popover-open')) tip.hidePopover();
  };
  const hideSoon = () => {
    clearTimeout(tipHide);
    tipHide = setTimeout(hideTip, 150);
  };
  window.grillTip = {
    hide: hideTip,
    // A re-render keeps the tip on its element, with its new words, or closes it.
    refresh: () => {
      if (!tipFor) return;
      const el = /** @type {HTMLElement} */ (tipFor);
      if (el.isConnected && el.dataset.tip) showTip(el);
      else hideTip();
    },
  };
  document.addEventListener('pointerover', (e) => {
    if (e.pointerType !== 'mouse') return;
    const el = tipped(e.target);
    if ((e.target instanceof Element && tip.contains(e.target)) || (el && el === tipFor)) {
      clearTimeout(tipHide);
      return;
    }
    if (!el) return;
    clearTimeout(tipHover);
    const [x, y] = [e.clientX, e.clientY];
    tipHover = setTimeout(() => {
      // A re-render may have replaced what the pointer rests on: take what is under it now.
      const now = tipped(document.elementFromPoint(x, y));
      if (now && !cardOpen()) showTip(now);
    }, 300);
  });
  document.addEventListener('pointerout', (e) => {
    if (e.pointerType !== 'mouse') return;
    const from = tipped(e.target);
    if (!from || (e.relatedTarget instanceof Node && from.contains(e.relatedTarget))) return;
    clearTimeout(tipHover);
    if (from === tipFor) hideSoon();
  });
  tip.addEventListener('pointerleave', (e) => { if (e.pointerType === 'mouse') hideSoon(); });
  document.addEventListener('focusin', (e) => {
    const el = tipped(e.target);
    if (el && el === e.target && el.matches(':focus-visible') && !cardOpen()) showTip(el);
  });
  document.addEventListener('focusout', (e) => { if (e.target === tipFor) hideTip(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
  // The pointer gone into a document frame sends no pointerout here.
  addEventListener('blur', hideTip);

  /**
   * grillLoadFailed says a module failed to load (a dropped connection, ERR_NETWORK_CHANGED): its
   * part of the page would otherwise do nothing, without a word. It shows once, and stays.
   */
  window.grillLoadFailed = () => {
    if (document.getElementById('load-failed')) return;
    const p = document.createElement('p');
    p.id = 'load-failed';
    p.className = 'toast load-failed';
    p.setAttribute('role', 'alert');
    const reload = document.createElement('button');
    reload.type = 'button';
    reload.className = 'btn-secondary';
    reload.textContent = 'Reload';
    reload.addEventListener('click', () => location.reload());
    p.append("Couldn't load, reload the page", reload);
    // A popover sits in the top layer: a sheet opened later must not cover the only way out.
    p.popover = 'manual';
    document.body.append(p);
    p.showPopover?.();
  };
  // A module graph that fails to fetch fires error on its script element, which does not bubble.
  // This file is a classic defer script ahead of every module script, so it listens in time.
  addEventListener('error', (e) => {
    if (e.target instanceof HTMLScriptElement && e.target.type === 'module') window.grillLoadFailed();
  }, true);
}
