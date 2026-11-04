// Control tower: sends a few GET /api/whoami per second, records who answered
// (release, instance, process) and counts every error. Separate file because of
// the strict CSP (no inline script). The state is exposed as window.tower.
(() => {
  'use strict';

  const TIMEOUT_MS = 3000;   // no answer after 3 s = error
  const GONE_MS = 15000;     // a process silent for 15 s while others answer has stopped
  const KEEP = 1500;         // requests kept for the ribbon
  const RED = '#f0506e';
  const $ = (id) => document.getElementById(id);
  const nf = new Intl.NumberFormat('fr-FR');

  const S = {
    started: Date.now(), sent: 0, ok: 0, err: 0, inflight: 0, lastOkAt: 0,
    entries: [],              // one per request: {t, done, ok, color, release, boot, ms, label}
    releases: new Map(),      // name → {name, version, color, commit, first, last, count}
    procs: new Map(),         // boot → {boot, release, version, color, instance, instanceName, commit, deployment, first, last, count, gone}
    events: [],               // {t, kind, text}
    switch: null,             // open switch-over: {from: [names], to, start, err0}
    switches: [],             // closed ones, for scripts
  };

  // ---------- requests ----------
  const rateSel = $('rate');
  let rate = clampRate(Number(new URLSearchParams(location.search).get('rate')) || 4);
  let timer = null;
  let paused = false;
  let procCount = 0;        // processes seen, to give each one of two bar heights

  function clampRate(r) { return [1, 2, 4, 8].includes(r) ? r : 4; }

  function start() {
    clearInterval(timer);
    if (!paused) timer = setInterval(send, 1000 / rate);
    $('lead-rate').textContent = rate;
  }

  function send() {
    const e = { t: Date.now(), done: false };
    S.entries.push(e);
    if (S.entries.length > KEEP) S.entries.splice(0, S.entries.length - KEEP);
    S.sent++; S.inflight++;
    const ctl = new AbortController();
    const to = setTimeout(() => ctl.abort(), TIMEOUT_MS);
    const t0 = performance.now();
    fetch('/api/whoami', { cache: 'no-store', signal: ctl.signal, headers: { Accept: 'application/json' } })
      .then(async (r) => {
        if (!r.ok) throw { label: 'HTTP ' + r.status };
        const j = await r.json().catch(() => null);
        if (!j || typeof j.name !== 'string' || typeof j.boot !== 'string') throw { label: 'réponse illisible' };
        return j;
      })
      .then((j) => onOk(e, j, performance.now() - t0))
      .catch((x) => onErr(e, x && x.label ? x.label : (x && x.name === 'AbortError' ? 'pas de réponse en 3 s' : 'erreur réseau')))
      .finally(() => { clearTimeout(to); S.inflight--; });
  }

  function onOk(e, j, ms) {
    const at = Date.now();
    Object.assign(e, { done: true, ok: true, color: safeColor(j.color), release: j.name, boot: j.boot, ms });
    S.ok++; S.lastOkAt = at;
    if (!S.procs.has(j.boot)) e.idx = procCount; else e.idx = S.procs.get(j.boot).idx;
    const commit = j.commit || j.built_from || '';

    let r = S.releases.get(j.name);
    if (!r) {
      r = { name: j.name, version: j.version, color: safeColor(j.color), commit, first: at, last: at, count: 0 };
      S.releases.set(j.name, r);
    }
    r.last = at; r.count++; if (commit) r.commit = commit;

    let p = S.procs.get(j.boot);
    if (!p) {
      const active = activeReleases();
      p = {
        boot: j.boot, release: j.name, version: j.version, color: safeColor(j.color),
        instance: j.instance || '', instanceName: j.instance_name || '', commit, deployment: j.deployment || '',
        first: at, last: at, count: 0, gone: false, idx: procCount++,
      };
      S.procs.set(j.boot, p);
      log(at, 'proc', `${j.name} (${j.version}) répond depuis un nouveau processus ${j.boot}${instText(p)}.`, p.color);
      if (active.length && !active.includes(j.name) && !S.switch) {
        S.switch = { from: active, to: j.name, start: at, err0: S.err };
        log(at, 'switch', `Bascule en cours : première réponse de ${j.name}, ${active.join(' et ')} répond encore.`, p.color);
      }
    }
    p.last = at; p.count++;
    if (p.gone) { p.gone = false; log(at, 'proc', `Le processus ${p.boot} (${p.release}) répond de nouveau.`, p.color); }
  }

  function onErr(e, label) {
    const at = Date.now();
    Object.assign(e, { done: true, ok: false, label });
    S.err++;
    const last = S.events[S.events.length - 1];
    if (last && last.kind === 'err' && last.label === label && at - last.until < 2000) {
      last.n++; last.until = at;
      last.text = `${last.n} erreurs « ${label} » entre ${clock(last.t)} et ${clock(at)}.`;
    } else {
      const ev = log(at, 'err', `Erreur : ${label}.`, RED);
      Object.assign(ev, { label, n: 1, until: at });
    }
  }

  // A process is gone when it has been silent for GONE_MS while others kept answering.
  function sweep() {
    const now = Date.now();
    for (const p of S.procs.values()) {
      if (!p.gone && S.lastOkAt - p.last > GONE_MS && now - p.last > GONE_MS) {
        p.gone = true;
        log(now, 'gone', `Le processus ${p.boot} (${p.release}) ne répond plus. Dernière réponse à ${clock(p.last)}.`, p.color);
      }
    }
    const sw = S.switch;
    if (sw && !activeReleases().some((n) => sw.from.includes(n))) {
      const lastOld = Math.max(...[...S.procs.values()].filter((p) => sw.from.includes(p.release)).map((p) => p.last));
      const firstNew = Math.min(...[...S.procs.values()].filter((p) => p.release === sw.to && p.first >= sw.start).map((p) => p.first));
      const errors = S.err - sw.err0;
      const overlap = (lastOld - firstNew) / 1000;
      const closed = { from: sw.from, to: sw.to, firstNew, lastOld, overlapSec: overlap, errors };
      S.switches.push(closed);
      S.switch = null;
      // A negative overlap only means the next request already went to the new
      // release: the gap is the time between two requests, not a time without answers.
      const how = overlap >= 0
        ? `les deux releases ont répondu en parallèle pendant ${overlap.toFixed(1).replace('.', ',')} s`
        : `dernière réponse de ${sw.from.join(' et ')} à ${clock(lastOld)}, première de ${sw.to} à ${clock(firstNew)}`;
      log(now, 'done', `Bascule ${sw.from.join(' + ')} → ${sw.to} terminée : ${how}, ${errors} erreur${errors > 1 ? 's' : ''} pendant la bascule.`, safeColor(S.releases.get(sw.to).color));
    }
  }

  function activeReleases() {
    const out = [];
    for (const p of S.procs.values()) if (!p.gone && !out.includes(p.release)) out.push(p.release);
    return out;
  }

  function log(t, kind, text, color) {
    const ev = { t, kind, text, color };
    S.events.push(ev);
    if (S.events.length > 300) S.events.shift();
    return ev;
  }

  // ---------- rendering ----------
  const canvas = $('ribbon');
  const ctx = canvas.getContext('2d');

  function render() {
    renderNow();
    renderHeadline();
    renderCounters();
    renderWho();
    renderLog();
    renderRibbon();
  }

  function renderHeadline() {
    const who = $('h-who');
    const names = activeReleases();
    who.replaceChildren();
    if (!names.length) {
      who.textContent = 'En attente';
      $('h-verb').textContent = 'de la première réponse.';
    } else {
      names.forEach((n, i) => {
        if (i) who.append(i === names.length - 1 ? ' et ' : ', ');
        const s = document.createElement('span');
        s.textContent = n;
        s.style.color = S.releases.get(n).color;
        who.append(s);
      });
      $('h-verb').textContent = names.length > 1 ? 'répondent.' : 'répond.';
    }
    const errEl = $('h-err');
    errEl.textContent = `${nf.format(S.err)} erreur${S.err > 1 ? 's' : ''}`;
    errEl.classList.toggle('is-bad', S.err > 0);
    const done = S.ok + S.err;
    $('h-total').textContent = `${nf.format(done)} requête${done > 1 ? 's' : ''}`;
  }

  // The latest notable event, under the headline: what the platform just did.
  function renderNow() {
    const ev = [...S.events].reverse().find((e) => e.kind !== 'proc' || S.procs.size > 1);
    if (!ev) return;
    const p = $('now');
    if (p.dataset.t === `${ev.t}${ev.text}`) return;
    p.dataset.t = `${ev.t}${ev.text}`;
    p.style.setProperty('--c', ev.color || '#9d9dbb');
    p.querySelector('time').textContent = clock(ev.t, true);
    p.querySelector('span').textContent = ev.text;
  }

  function renderCounters() {
    $('c-sent').textContent = nf.format(S.sent);
    $('c-ok').textContent = nf.format(S.ok);
    $('c-err').textContent = nf.format(S.err);
    $('c-err').parentElement.classList.toggle('is-bad', S.err > 0);
    const since = Date.now() - 60000;
    const ms = S.entries.filter((e) => e.ok && e.t >= since).map((e) => e.ms).sort((a, b) => a - b);
    $('c-p95').textContent = ms.length ? `${Math.round(ms[Math.min(ms.length - 1, Math.floor(ms.length * 0.95))])} ms` : '—';
    $('c-inst').textContent = [...S.procs.values()].filter((p) => !p.gone).length;
  }

  function renderWho() {
    const rows = [...S.procs.values()].sort((a, b) => (a.gone - b.gone) || (b.first - a.first)).slice(0, 8);
    const tb = $('who');
    if (!rows.length) return;
    tb.replaceChildren(...rows.map((p) => {
      const tr = document.createElement('tr');
      if (p.gone) tr.className = 'is-gone';
      tr.append(
        td(chip(p.release, p.version, p.color)),
        td(text(p.instance || p.instanceName ? `#${p.instance} · ${p.instanceName}` : '—'), 'inst'),
        td(commitCell(p.commit)),
        td(text(nf.format(p.count)), 'num'),
        td(text(`${clock(p.first, true)} → ${clock(p.last, true)}`), 'mono'),
        td(text(p.gone ? 'arrêté' : 'en service'), p.gone ? 'state' : 'state is-live'),
      );
      return tr;
    }));
  }

  function renderLog() {
    const ol = $('log');
    const evs = S.events.slice(-40).reverse();
    if (ol.dataset.n === `${S.events.length}:${evs[0] ? evs[0].text : ''}`) return;
    ol.dataset.n = `${S.events.length}:${evs[0] ? evs[0].text : ''}`;
    ol.replaceChildren(...evs.map((ev) => {
      const li = document.createElement('li');
      li.className = 'is-' + ev.kind;
      li.style.setProperty('--c', ev.color || '#9d9dbb');
      const time = document.createElement('time');
      time.textContent = clock(ev.t);
      li.append(time, text(ev.text));
      return li;
    }));
  }

  function renderRibbon() {
    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth, h = canvas.clientHeight;
    if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
      canvas.width = Math.round(w * dpr); canvas.height = Math.round(h * dpr);
    }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const pitch = w < 600 ? 4 : 5;
    const bar = pitch - 1;
    const n = Math.floor(w / pitch);
    const list = S.entries.slice(-n);
    const top = 18; // room for the release labels
    let prev = null;
    let lastLabel = -Infinity; // one label per 80 px: an alternation of releases stays readable
    list.forEach((e, i) => {
      const x = w - (list.length - i) * pitch;
      let bh, color;
      if (!e.done) { bh = (h - top) * 0.16; color = 'rgba(222,221,238,0.28)'; }
      else if (!e.ok) { bh = h; color = RED; }
      else { bh = (h - top) * (e.idx % 2 ? 0.5 : 0.72); color = e.color; }
      ctx.fillStyle = color;
      ctx.fillRect(x, h - bh, bar, bh);
      if (e.ok) {
        if (prev && prev !== e.release && x - lastLabel > 80) {
          lastLabel = x;
          ctx.fillStyle = 'rgba(249,249,251,0.85)';
          ctx.fillRect(x - 1, top - 4, 1, h - top + 4);
          ctx.font = '600 11px "Plus Jakarta Sans", system-ui, sans-serif';
          ctx.fillStyle = e.color;
          ctx.fillText(e.release, Math.min(x + 3, w - ctx.measureText(e.release).width - 2), 11);
        }
        prev = e.release;
      }
    });
    const oldest = list[0];
    $('ax-left').textContent = oldest ? `il y a ${Math.round((Date.now() - oldest.t) / 1000)} s` : '—';
  }

  // ---------- helpers ----------
  function instText(p) {
    if (!p.instance && !p.instanceName) return '';
    return `, instance #${p.instance}${p.instanceName ? ' · ' + p.instanceName : ''}`;
  }
  function safeColor(c) { return /^#[0-9a-fA-F]{6}$/.test(c) ? c : '#deddee'; }
  function clock(t, short) {
    const d = new Date(t);
    const hms = d.toLocaleTimeString('fr-FR', { hour12: false });
    return short ? hms : `${hms}.${String(d.getMilliseconds()).padStart(3, '0')}`;
  }
  function td(child, cls) { const c = document.createElement('td'); if (cls) c.className = cls; c.append(child); return c; }
  function text(s) { return document.createTextNode(s); }
  function chip(name, version, color) {
    const s = document.createElement('span');
    s.className = 'zd-chip';
    s.style.setProperty('--v', color);
    s.textContent = `${name} · ${version}`;
    return s;
  }
  function commitCell(commit) {
    if (!commit) return text('—');
    const wrap = document.createElement('span');
    wrap.className = 'zd-commit';
    const code = document.createElement('code');
    code.textContent = commit.slice(0, 7);
    code.title = commit;
    wrap.append(code);
    if (navigator.clipboard && commit.length > 7) {
      const b = document.createElement('button');
      b.type = 'button'; b.className = 'zd-copy'; b.textContent = 'copier';
      b.setAttribute('aria-label', `Copier le commit ${commit}`);
      b.addEventListener('click', () => navigator.clipboard.writeText(commit).then(() => { b.textContent = 'copié'; }));
      wrap.append(b);
    }
    return wrap;
  }

  // ---------- controls ----------
  rateSel.value = String(rate);
  rateSel.addEventListener('change', () => { rate = clampRate(Number(rateSel.value)); start(); });
  $('pause').addEventListener('click', (ev) => {
    paused = !paused;
    ev.currentTarget.textContent = paused ? 'Reprendre' : 'Pause';
    start();
  });
  $('reset').addEventListener('click', () => {
    Object.assign(S, { started: Date.now(), sent: 0, ok: 0, err: 0, lastOkAt: 0, entries: [], events: [], switch: null, switches: [] });
    S.releases.clear(); S.procs.clear();
    $('who').replaceChildren();
  });
  window.addEventListener('resize', renderRibbon);

  // Scripts (and the README screenshots) read the tower through this object.
  window.tower = {
    state: S,
    summary: () => ({
      sent: S.sent, ok: S.ok, err: S.err, inflight: S.inflight,
      releases: [...S.releases.values()],
      procs: [...S.procs.values()],
      switches: S.switches,
      events: S.events.map((e) => ({ t: new Date(e.t).toISOString(), kind: e.kind, text: e.text })),
      errors: S.entries.filter((e) => e.done && !e.ok).map((e) => ({ t: new Date(e.t).toISOString(), label: e.label })),
    }),
  };

  start();
  setInterval(sweep, 1000);
  setInterval(render, 250);
  render();
})();
