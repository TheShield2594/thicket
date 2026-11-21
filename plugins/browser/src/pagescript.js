'use strict';
// Runs in the page (page.evaluate): numbers the things a sprig can act on
// with a data-thicket-ref attribute and returns them with the page's text.
// Only reads the page; the attribute is the one change it makes.

function snapshot(maxElements) {
  const sel = [
    'a[href]', 'button', 'input:not([type=hidden])', 'select', 'textarea', 'summary',
    '[role=button]', '[role=link]', '[role=checkbox]', '[role=radio]', '[role=tab]', '[role=menuitem]',
    '[role=option]', '[role=switch]', '[role=combobox]', '[role=textbox]', '[contenteditable=""]', '[contenteditable=true]',
  ].join(',');
  for (const el of document.querySelectorAll('[data-thicket-ref]')) el.removeAttribute('data-thicket-ref');
  const visible = (el) => {
    if (!el.getClientRects().length) return false;
    const st = getComputedStyle(el);
    return st.visibility !== 'hidden' && st.display !== 'none' && Number(st.opacity) !== 0;
  };
  const clip = (s, n) => {
    s = String(s || '').replace(/\s+/g, ' ').trim();
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  };
  const labelOf = (el) => {
    const aria = el.getAttribute('aria-label');
    if (aria) return aria;
    const by = el.getAttribute('aria-labelledby');
    if (by) {
      const t = by.split(/\s+/).map((id) => document.getElementById(id)).filter(Boolean).map((n) => n.innerText).join(' ');
      if (t.trim()) return t;
    }
    if (el.labels && el.labels.length) return [...el.labels].map((l) => l.innerText).join(' ');
    if (el.tagName === 'INPUT' && ['submit', 'button', 'reset'].includes(el.type)) return el.value;
    const txt = el.innerText;
    if (txt && txt.trim()) return txt;
    return el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('alt') || el.getAttribute('name') || '';
  };
  const out = [];
  let n = 0;
  for (const el of document.querySelectorAll(sel)) {
    if (out.length >= maxElements) break;
    if (!visible(el) || el.disabled) continue;
    n++;
    el.setAttribute('data-thicket-ref', String(n));
    const tag = el.tagName.toLowerCase();
    const item = { ref: n, kind: el.getAttribute('role') || tag, label: clip(labelOf(el), 80) };
    if (tag === 'input') {
      item.kind = 'input';
      item.type = el.type;
      if (el.type === 'password') item.value = el.value ? '(filled)' : '';
      else if (el.type === 'checkbox' || el.type === 'radio') item.checked = el.checked;
      else if (el.type !== 'file') item.value = clip(el.value, 60);
      if (el.getAttribute('autocomplete')) item.autocomplete = el.getAttribute('autocomplete');
    } else if (tag === 'textarea') {
      item.value = clip(el.value, 60);
    } else if (tag === 'select') {
      item.options = [...el.options].slice(0, 25).map((o) => clip(o.label || o.value, 40));
      item.value = el.selectedOptions[0] ? clip(el.selectedOptions[0].label, 40) : '';
    } else if (tag === 'a') {
      item.href = clip(el.href, 200);
    }
    out.push(item);
  }
  const payment = !!document.querySelector('input[autocomplete^="cc-"], input[name*="cardnumber" i], input[name*="card_number" i], input[id*="cardnumber" i]');
  const password = !!document.querySelector('input[type=password]');
  return {
    title: document.title || '',
    url: location.href,
    text: document.body ? document.body.innerText : '',
    elements: out,
    payment,
    password,
  };
}

module.exports = { snapshot };
