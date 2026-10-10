'use strict';

// Messages from a document frame are untrusted: the frame runs uploaded scripts, which can post
// anything. validate returns a fresh copy of a well-formed message, or null. The limits match
// internal/anchor; the server checks again.
const GrillBridge = (() => {
  const MAX_EXACT = 4000;
  const MAX_CONTEXT = 64;
  const MAX_FINGERPRINT = 128;
  const MAX_STEPS = 64;
  const TAG = /^[a-zA-Z][a-zA-Z0-9-]{0,31}$/;
  const PATH = /^body(?: > [a-zA-Z][a-zA-Z0-9-]{0,31}:nth-of-type\([1-9][0-9]{0,3}\))*$/;

  const plain = (v) => v !== null && typeof v === 'object' && Object.getPrototypeOf(v) === Object.prototype;
  const only = (v, keys) => Object.keys(v).every((k) => keys.includes(k));
  const str = (v, max) => typeof v === 'string' && v.length <= max;

  function anchor(a) {
    if (!plain(a)) return null;
    if (a.kind === 'text') {
      if (!only(a, ['kind', 'exact', 'prefix', 'suffix'])) return null;
      if (!str(a.exact, MAX_EXACT) || !a.exact.trim()) return null;
      if (!str(a.prefix ?? '', MAX_CONTEXT) || !str(a.suffix ?? '', MAX_CONTEXT)) return null;
      return { kind: 'text', exact: a.exact, prefix: a.prefix ?? '', suffix: a.suffix ?? '' };
    }
    if (a.kind === 'element') {
      if (!only(a, ['kind', 'path', 'tag', 'fingerprint'])) return null;
      if (!str(a.tag, 32) || !TAG.test(a.tag) || !str(a.path, 4096) || !PATH.test(a.path)) return null;
      if (a.path.split(' > ').length - 1 > MAX_STEPS) return null;
      if (a.path.split(' > ').pop().split(':')[0] !== a.tag) return null;
      if (!str(a.fingerprint ?? '', MAX_FINGERPRINT)) return null;
      return { kind: 'element', path: a.path, tag: a.tag, fingerprint: a.fingerprint ?? '' };
    }
    return null;
  }

  function validate(data) {
    if (!plain(data)) return null;
    switch (data.type) {
      case 'gr:ready':
        return only(data, ['type']) ? { type: data.type } : null;
      case 'gr:anchor': {
        const a = only(data, ['type', 'anchor']) ? anchor(data.anchor) : null;
        return a && { type: data.type, anchor: a };
      }
      case 'gr:open':
        return only(data, ['type', 'id']) && Number.isSafeInteger(data.id) && data.id > 0 ? { type: data.type, id: data.id } : null;
      case 'gr:pick':
        return only(data, ['type', 'on']) && data.on === false ? { type: data.type, on: false } : null;
      default:
        return null;
    }
  }

  return { validate };
})();
