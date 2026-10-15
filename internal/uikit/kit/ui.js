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

  /** @type {WeakMap<HTMLButtonElement, number>} */
  const cooldowns = new WeakMap();
  /**
   * grillCooldown keeps button disabled for seconds, counting down in its label (011-AC21,
   * 011-AC22): a resend within a minute would only meet the server's rate limit. A new call on a
   * button still counting replaces its countdown.
   * @param {HTMLButtonElement} button
   * @param {number} seconds
   */
  window.grillCooldown = (button, seconds) => {
    clearTimeout(cooldowns.get(button));
    const label = button.dataset.label || button.textContent || '';
    button.dataset.label = label;
    button.disabled = true;
    const tick = () => {
      if (seconds <= 0) {
        cooldowns.delete(button);
        button.textContent = label;
        button.disabled = false;
        delete button.dataset.label;
        return;
      }
      button.textContent = label + ' (' + seconds + ' s)';
      seconds--;
      cooldowns.set(button, setTimeout(tick, 1000));
    };
    tick();
  };

  /**
   * grillDuration words a link lifetime in seconds as the server's mails do (011-AC24): "1 minute",
   * "N minutes", else Go's time.Duration form ("4s", "1m30s").
   * @param {number} s
   * @returns {string}
   */
  window.grillDuration = (s) => {
    if (s === 60) return '1 minute';
    if (s > 0 && s % 60 === 0) return s / 60 + ' minutes';
    const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
    return (h ? h + 'h' : '') + (h || m ? m + 'm' : '') + (s % 60) + 's';
  };

  /**
   * grillLinkError says why a request for an emailed link failed, with the next step (011-AC28).
   * @param {{reason?: string, error?: string, retry_after_s?: number} | null} data null when the network failed
   * @param {number} status
   * @returns {string}
   */
  const grillLinkError = (data, status) => {
    if (!data) return 'No connection. Check your network and try again.';
    if (data.reason === 'rate_limited') return 'Too many requests. Try again in ' + Math.max(1, Math.ceil((data.retry_after_s || 60) / 60)) + ' min.';
    if (data.reason === 'invalid_email') return 'Check the address: it does not look like an email.';
    return data.error || 'Something went wrong (HTTP ' + status + '). Try again.';
  };

  /**
   * grillLinkForm turns form, one email field and its submit button, into a request for an emailed
   * link with feedback in place (011-AC28): a busy button, then a panel in the form's place saying
   * where the link went, how long it works, Resend with a countdown and "Use another address";
   * errors show under the field. show(email, ttl, cool) opens the panel for a link already pending,
   * Resend waiting cool seconds.
   * @param {{form: HTMLFormElement, url: string, kind: 'bind' | 'signin', ttl: () => number,
   *   onSent?: (email: string) => void, onOther?: () => void}} opts
   */
  window.grillLinkForm = ({ form, url, kind, ttl, onSent, onOther }) => {
    const input = /** @type {HTMLInputElement} */ (form.querySelector('input[type=email]'));
    const submit = /** @type {HTMLButtonElement} */ (form.querySelector('button:not([type=button])'));
    const error = document.createElement('p');
    error.className = 'field-error';
    error.setAttribute('role', 'alert');
    // After the label when it wraps the field: inside it, the error would join the field's name.
    (input.closest('label') || input).after(error);
    input.setAttribute('aria-describedby', (input.id || 'email') + '-error');
    error.id = (input.id || 'email') + '-error';

    const panel = document.createElement('div');
    panel.className = 'link-sent';
    panel.setAttribute('role', 'status');
    panel.hidden = true;
    const head = document.createElement('h3');
    head.tabIndex = -1;
    const life = document.createElement('p');
    const spam = document.createElement('p');
    spam.className = 'hint';
    spam.textContent = 'Check your spam folder if it does not arrive.';
    const again = document.createElement('p');
    again.className = 'again';
    again.tabIndex = -1;
    again.textContent = 'Sent again';
    const panelError = document.createElement('p');
    panelError.className = 'field-error';
    panelError.setAttribute('role', 'alert');
    const actions = document.createElement('div');
    actions.className = 'actions';
    const resend = document.createElement('button');
    resend.type = 'button';
    resend.className = 'btn-secondary';
    resend.textContent = 'Resend';
    const other = document.createElement('button');
    other.type = 'button';
    other.className = 'btn-ghost';
    other.textContent = 'Use another address';
    actions.append(resend, other);
    panel.append(head, life, spam, again, panelError, actions);
    form.after(panel);
    let sentTo = '';

    /** @param {HTMLButtonElement} b @param {boolean} on */
    const busy = (b, on) => {
      if (on) {
        b.dataset.idle = b.textContent || '';
        b.textContent = 'Sending…';
        b.setAttribute('aria-busy', 'true');
      } else {
        b.textContent = b.dataset.idle || b.textContent;
        b.removeAttribute('aria-busy');
      }
      b.disabled = on;
    };
    /** @param {string} email @returns {Promise<{ok: boolean, data: any, status: number}>} */
    const ask = async (email) => {
      try {
        const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ email }) });
        return { ok: res.ok, data: await res.json().catch(() => ({})), status: res.status };
      } catch {
        return { ok: false, data: null, status: 0 };
      }
    };
    /** @param {string} email @param {number} seconds @param {number} cool seconds left before Resend */
    const show = (email, seconds, cool) => {
      sentTo = email;
      head.textContent = kind === 'bind' ? 'Link sent to ' + email : 'If a profile uses this address, a link is on its way';
      life.textContent = 'The link works once, within ' + window.grillDuration(seconds || ttl()) + '.';
      again.hidden = true;
      panelError.textContent = '';
      form.hidden = true;
      panel.hidden = false;
      if (cool > 0) window.grillCooldown(resend, cool);
    };
    const reset = () => {
      panel.hidden = true;
      form.hidden = false;
      error.textContent = '';
    };
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const email = input.value.trim();
      error.textContent = '';
      busy(submit, true);
      const { ok, data, status } = await ask(email);
      busy(submit, false);
      // The busy button dropped focus: it goes where the answer is.
      if (!ok) {
        error.textContent = grillLinkError(data, status);
        input.focus();
        return;
      }
      if (onSent) onSent(email);
      show(email, data.ttl_s, 60);
      head.focus();
    });
    resend.addEventListener('click', async () => {
      again.hidden = true;
      panelError.textContent = '';
      busy(resend, true);
      const { ok, data, status } = await ask(sentTo);
      busy(resend, false);
      if (!ok) {
        panelError.textContent = grillLinkError(data, status);
        resend.focus();
        return;
      }
      again.hidden = false;
      window.grillCooldown(resend, 60);
      again.focus();
    });
    other.addEventListener('click', () => {
      const was = sentTo;
      if (onOther) onOther();
      reset();
      input.value = was;
      input.focus();
    });
    return { show: (/** @type {string} */ email, /** @type {number} */ seconds, cool = 0) => show(email, seconds, cool), reset };
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
    pulse: '<path d="M22 12h-4l-3 9L9 3l-3 9H2"/>',
    profile: '<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/>',
    download: '<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3"/>',
    trash: '<path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/>',
    copy: '<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>',
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
    // Touch has no hover: a page shows a tip on a first tap where one matters (011-AC7).
    show: showTip,
    shownFor: () => tipFor,
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
   * grillToast says what an action did, 4 s, bottom right (docs/design.md §5). It shares the page's
   * #toasts region when there is one (the room page's), and #toast-live for screen readers.
   * @param {string} text
   */
  window.grillToast = (text) => {
    let region = document.getElementById('toasts');
    if (!region) {
      region = Object.assign(document.createElement('div'), { id: 'toasts', className: 'toasts' });
      // A popover sits in the top layer: a toast shows over an open sheet too.
      region.popover = 'manual';
      document.body.append(region);
    }
    const t = Object.assign(document.createElement('p'), { className: 'toast', textContent: text });
    region.append(t);
    const live = document.getElementById('toast-live');
    if (live) live.textContent = text;
    if (!region.matches(':popover-open')) region.showPopover();
    setTimeout(() => {
      t.remove();
      if (region && !region.children.length && region.matches(':popover-open')) region.hidePopover();
    }, 4000);
  };

  // A .value's copy button copies the whole value, not the cut text on screen.
  document.addEventListener('click', (e) => {
    const b = e.target instanceof Element ? e.target.closest('.value .copy') : null;
    const v = /** @type {HTMLElement | null} */ (b?.closest('.value'));
    if (!v) return;
    (navigator.clipboard?.writeText(v.dataset.value || '') ?? Promise.reject()).then(() => window.grillToast('Copied'), () => window.grillToast("Couldn't copy: select the text instead"));
  });

  /**
   * grillShowTab scrolls a tab bar that overflows so its current tab shows whole, clear of the fade,
   * with a whole tab at its left edge: a scroll that only just reveals the current tab cuts the one
   * before ("ooms" for Rooms). When that tab start lies past the scroll end, an end padding makes it
   * reachable.
   * @param {Element | null} nav
   */
  window.grillShowTab = (nav) => {
    const cur = nav?.querySelector('[aria-current=page]');
    if (!(nav instanceof HTMLElement) || !cur) return;
    nav.style.paddingInlineEnd = '';
    const max = nav.scrollWidth - nav.clientWidth;
    if (max <= 0) return;
    const at = (/** @type {Element} */ a) => a.getBoundingClientRect().left - nav.getBoundingClientRect().left - nav.clientLeft + nav.scrollLeft;
    const fade = matchMedia('(max-width: 40rem)').matches ? 24 : 0;
    const [left, right] = [at(cur), at(cur) + cur.getBoundingClientRect().width];
    // At the scroll end the fade is gone (tokens.css).
    const fits = (/** @type {number} */ s) => left >= s - 0.5 && right <= s + nav.clientWidth - (s < max - 0.5 ? fade : 0) + 0.5;
    const s = [...nav.children].map(at).find((x) => x >= -0.5 && fits(Math.min(x, max))) ?? left;
    if (s > max) nav.style.paddingInlineEnd = s - max + 'px';
    nav.scrollLeft = s;
  };

  // A ⋯ menu (.menu.row-menu) hangs under its button, its right edge on the button's, and above it
  // at the bottom of the screen.
  document.addEventListener('toggle', (e) => {
    const m = /** @type {HTMLElement} */ (e.target);
    if (/** @type {ToggleEvent} */ (e).newState !== 'open' || !m.classList?.contains('row-menu') || !m.id) return;
    const by = document.querySelector('[popovertarget="' + CSS.escape(m.id) + '"]');
    if (!by) return;
    const r = by.getBoundingClientRect();
    m.style.left = Math.max(8, Math.min(r.right - m.offsetWidth, innerWidth - 8 - m.offsetWidth)) + 'px';
    m.style.top = (r.bottom + 4 + m.offsetHeight > innerHeight - 8 ? Math.max(8, r.top - 4 - m.offsetHeight) : r.bottom + 4) + 'px';
  }, true);

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
