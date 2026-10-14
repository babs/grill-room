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
