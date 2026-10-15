'use strict';
// Shared by every page (docs/design.md, 005-AC39): the confirm sheet that replaces the browser's
// confirm dialog, and the coarse "2 min ago". A page loads it as a classic script; its modules
// call window.grillConfirm and window.grillAgo.

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
