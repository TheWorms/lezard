(() => {
  'use strict';

  const NS = 'http://www.w3.org/2000/svg';
  const RANGES = [
    ['1h', '1 h', 'la dernière heure'],
    ['6h', '6 h', 'les 6 dernières heures'],
    ['24h', '24 h', 'les 24 dernières heures'],
    ['3d', '3 j', 'les 3 derniers jours'],
    ['7d', '7 j', 'les 7 derniers jours'],
    ['30d', '30 j', 'les 30 derniers jours'],
    ['90d', '90 j', 'les 90 derniers jours'],
    ['1y', '1 an', 'la dernière année'],
  ];
  const LABELS = { ok: 'OK', warning: 'Attention', critical: 'Critique', offline: 'Hors ligne' };
  const REFRESH_MS = 30000;

  const $ = (id) => document.getElementById(id);
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* stockage indisponible */ } },
  };

  const state = {
    range: '24h',
    host: null,
    view: 'chart',
    data: null,
    hist: null,
    seqHosts: 0,
    seqHist: 0,
    thr: store.get('lezard.thr') !== '0',
  };
  const savedRange = store.get('lezard.range');
  if (RANGES.some((r) => r[0] === savedRange)) state.range = savedRange;
  state.host = store.get('lezard.host');

  // ---------- helpers DOM ----------
  function h(tag, props, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (k === 'class') e.className = v;
      else if (k === 'text') e.textContent = v;
      else e.setAttribute(k, v);
    }
    for (const c of kids.flat()) if (c != null) e.append(c);
    return e;
  }
  function sv(tag, attrs, ...kids) {
    const e = document.createElementNS(NS, tag);
    for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, String(v));
    for (const c of kids.flat()) if (c != null) e.append(c);
    return e;
  }

  const ICONS = {
    ok: [['circle', { cx: 8, cy: 8, r: 6.5 }], ['path', { d: 'M5 8.2l2 2 4-4.4' }]],
    warning: [['path', { d: 'M8 2.2 14.2 13H1.8z' }], ['path', { d: 'M8 6.5v3.2M8 11.5v.1' }]],
    critical: [['path', { d: 'M5.2 1.8h5.6l3.4 3.4v5.6l-3.4 3.4H5.2l-3.4-3.4V5.2z' }], ['path', { d: 'M6 6l4 4M10 6l-4 4' }]],
    offline: [['circle', { cx: 8, cy: 8, r: 6.5 }], ['path', { d: 'M5.5 8h5' }]],
  };
  function icon(status) {
    const svg = sv('svg', {
      class: 'ico', viewBox: '0 0 16 16', width: 16, height: 16, 'aria-hidden': 'true',
      fill: 'none', stroke: 'currentColor', 'stroke-width': 1.6, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    });
    for (const [t, a] of ICONS[status] || ICONS.offline) svg.append(sv(t, a));
    return svg;
  }
  function badge(status) {
    return h('span', { class: 'badge st-' + status }, icon(status), LABELS[status] || status);
  }

  // ---------- formats ----------
  const nf1 = new Intl.NumberFormat('fr-FR', { minimumFractionDigits: 1, maximumFractionDigits: 1 });
  const fmt = (v) => nf1.format(v);
  const pad2 = (n) => String(n).padStart(2, '0');
  const dayFmt = new Intl.DateTimeFormat('fr-FR', { weekday: 'short', day: 'numeric', month: 'short' });
  const dayShort = new Intl.DateTimeFormat('fr-FR', { day: 'numeric', month: 'short' });
  const hm = (d) => pad2(d.getHours()) + ':' + pad2(d.getMinutes());

  function fmtTime(t, withDate) {
    const d = new Date(t * 1000);
    return withDate ? dayFmt.format(d) + ' ' + hm(d) : hm(d);
  }
  function ageText(sec) {
    if (sec < 5) return 'à l’instant';
    if (sec < 60) return 'il y a ' + Math.round(sec) + ' s';
    if (sec < 3600) return 'il y a ' + Math.round(sec / 60) + ' min';
    if (sec < 86400) return 'il y a ' + Math.round(sec / 3600) + ' h';
    return 'il y a ' + Math.round(sec / 86400) + ' j';
  }
  function stepText(sec) {
    if (sec < 60) return sec + ' s';
    if (sec < 3600) return Math.round(sec / 60) + ' min';
    if (sec < 86400) return Math.round(sec / 3600) + ' h';
    return Math.round(sec / 86400) + ' j';
  }
  const colorVar = (n) => 'var(--s' + (n >= 1 && n <= 12 ? n : 1) + ')';
  const hostColor = (host) => {
    const x = state.data && state.data.hosts.find((e) => e.host === host);
    return colorVar(x ? x.color : 1);
  };
  const DASHES = ['', '7 4', '2 4', '9 4 2 4'];
  const sensorLabel = (s) => (s === 'cpu' ? 'CPU' : s);
  const rangeInfo = () => RANGES.find((r) => r[0] === state.range) || RANGES[2];

  async function getJSON(url) {
    const r = await fetch(url, { cache: 'no-store' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.json();
  }

  // ---------- thème ----------
  $('theme').addEventListener('click', () => {
    const root = document.documentElement;
    const next = root.dataset.theme === 'light' ? 'dark' : 'light';
    root.dataset.theme = next;
    store.set('lezard.theme', next);
  });

  // ---------- filtre de période ----------
  function renderRanges() {
    const box = $('ranges');
    box.replaceChildren();
    for (const [key, label] of RANGES) {
      const b = h('button', { type: 'button', 'aria-pressed': String(key === state.range), text: label });
      b.addEventListener('click', () => {
        if (state.range === key) return;
        state.range = key;
        store.set('lezard.range', key);
        renderRanges();
        $('cards').classList.add('loading');
        $('chart').classList.add('loading');
        refresh();
      });
      box.append(b);
    }
  }

  // ---------- bandeau, résumé ----------
  function banner(show, text) {
    const b = $('banner');
    b.hidden = !show;
    if (show) b.textContent = text;
  }

  function renderSummary(d) {
    const box = $('summary');
    box.replaceChildren();
    if (!d.hosts.length) return;
    box.append(h('span', { class: 'pill' }, h('b', { text: String(d.hosts.length) }), d.hosts.length > 1 ? 'machines' : 'machine'));
    for (const st of ['critical', 'warning', 'offline', 'ok']) {
      const n = d.summary[st] || 0;
      if (!n) continue;
      box.append(h('span', { class: 'pill st-' + st }, icon(st), h('b', { text: String(n) }), LABELS[st]));
    }
  }

  // ---------- cartes ----------
  function spark(points) {
    const wrap = h('div', { class: 'spark' });
    if (!points || points.length < 2) {
      wrap.append(h('span', { class: 'muted small', text: 'Pas assez de relevés' }));
      return wrap;
    }
    const W = 200, H = 48, padY = 5;
    let lo = Infinity, hi = -Infinity;
    for (const p of points) { lo = Math.min(lo, p[1]); hi = Math.max(hi, p[1]); }
    if (hi - lo < 6) { const mid = (hi + lo) / 2; lo = mid - 3; hi = mid + 3; }
    const t0 = points[0][0], t1 = points[points.length - 1][0];
    const px = (t) => (t1 === t0 ? W : ((t - t0) / (t1 - t0)) * W);
    const py = (v) => H - padY - ((v - lo) / (hi - lo)) * (H - 2 * padY);
    let d = '';
    points.forEach((p, i) => { d += (i ? 'L' : 'M') + px(p[0]).toFixed(1) + ' ' + py(p[1]).toFixed(1); });
    wrap.append(sv('svg', { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' }, sv('path', { d })));
    const last = points[points.length - 1];
    const dot = h('span', { class: 'dot' });
    dot.style.left = (px(last[0]) / W * 100).toFixed(2) + '%';
    dot.style.top = (py(last[1]) / H * 100).toFixed(2) + '%';
    wrap.append(dot);
    return wrap;
  }

  function card(host, now) {
    const primary = host.sensors[0];
    const b = h('button', { class: 'card st-' + host.status, type: 'button', 'aria-pressed': String(host.host === state.host), 'data-host': host.host });
    b.style.setProperty('--hc', colorVar(host.color));
    b.append(h('div', { class: 'card-top' }, h('span', { class: 'card-name' }, h('span', { class: 'sw', 'aria-hidden': 'true' }), h('span', { class: 'nm', text: host.name })), badge(host.status)));
    b.append(h('div', { class: 'temp' }, fmt(host.temp), h('span', { class: 'unit', text: '°C' })));
    b.append(spark(primary.spark));
    if (primary.min != null) {
      b.append(h('div', { class: 'stats' },
        'min ', h('b', { text: fmt(primary.min) }), ' · moy ', h('b', { text: fmt(primary.avg) }), ' · max ', h('b', { text: fmt(primary.max) }),
        ' sur ' + rangeInfo()[1]));
    }
    if (host.sensors.length > 1) {
      b.append(h('div', { class: 'others', text: host.sensors.slice(1).map((s) => sensorLabel(s.sensor) + ' ' + fmt(s.temp) + ' °C').join(' · ') }));
    }
    b.append(h('div', { class: 'meta' },
      h('span', { text: host.status === 'offline' ? 'Vu ' + ageText(now - host.last) : ageText(now - host.last) }),
      h('span', { text: 'seuils ' + host.warn + ' / ' + host.crit + ' °C' })));
    b.addEventListener('click', () => selectHost(host.host));
    return b;
  }

  function renderCards(d) {
    const box = $('cards');
    const focused = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.host : null;
    box.replaceChildren(...d.hosts.map((x) => card(x, d.now)));
    box.classList.remove('loading');
    if (focused) {
      const again = box.querySelector('[data-host="' + CSS.escape(focused) + '"]');
      if (again) again.focus({ preventScroll: true });
    }
    const empty = !d.hosts.length;
    $('empty').hidden = !empty;
    box.hidden = empty;
    if (empty) {
      $('empty-cmd').textContent = 'curl -fsS -X POST \\\n  -H "Authorization: Bearer $LEZARD_TOKEN" \\\n  -d host=$(hostname) -d temp=52.3 \\\n  ' + location.origin + '/api/push';
    }
  }

  function selectHost(name) {
    state.host = name;
    store.set('lezard.host', name);
    document.querySelectorAll('.card').forEach((c) => c.setAttribute('aria-pressed', String(c.dataset.host === name)));
    $('chart').classList.add('loading');
    $('detail').hidden = false;
    $('detail').scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'nearest' });
    loadHistory();
  }

  // ---------- historique et graphique ----------
  async function loadHistory() {
    const d = state.data;
    const detail = $('detail');
    if (!d || !d.hosts.length) { detail.hidden = true; return; }
    if (!d.hosts.some((x) => x.host === state.host)) state.host = d.hosts[0].host;
    detail.hidden = false;
    const seq = ++state.seqHist;
    try {
      const hist = await getJSON('/api/history?host=' + encodeURIComponent(state.host) + '&range=' + state.range);
      if (seq !== state.seqHist) return;
      state.hist = hist;
      renderDetail();
    } catch (e) {
      if (seq !== state.seqHist) return;
      $('chart').classList.remove('loading');
      banner(true, 'Impossible de charger l’historique de cette machine.');
    }
  }

  $('view-toggle').addEventListener('click', () => {
    state.view = state.view === 'chart' ? 'table' : 'chart';
    renderDetail();
  });

  function renderDetail() {
    const hist = state.hist;
    if (!hist) return;
    const series = hist.sensors.filter((s) => s.rows.length);
    $('detail-title').textContent = hist.name + ' — ' + rangeInfo()[2];
    const multi = series.length >= 2;
    const lg = $('legend');
    lg.replaceChildren();
    if (multi) {
      series.forEach((s, i) => {
        const key = sv('svg', { width: 20, height: 6, viewBox: '0 0 20 6', 'aria-hidden': 'true' },
          sv('line', { x1: 0, x2: 20, y1: 3, y2: 3, stroke: hostColor(hist.host), 'stroke-width': 2, 'stroke-dasharray': DASHES[i % 4] }));
        lg.append(h('span', {}, key, sensorLabel(s.sensor)));
      });
    }
    $('thr-toggle').setAttribute('aria-pressed', String(state.thr));
    const asTable = state.view === 'table';
    const tb = $('view-toggle');
    tb.setAttribute('aria-pressed', String(asTable));
    tb.textContent = asTable ? 'Vue graphique' : 'Vue tableau';
    $('chart').hidden = asTable;
    $('table').hidden = !asTable;
    $('chart').classList.remove('loading');
    if (asTable) drawTable(series); else drawChart(hist, series);
    $('detail-foot').textContent = series.length
      ? 'Chaque point est la moyenne sur ' + stepText(hist.step) + (multi ? '' : ' ; la zone teintée va du minimum au maximum') + '. Seuils : attention ' + hist.warn + ' °C, critique ' + hist.crit + ' °C.'
      : '';
  }

  function niceTicks(lo, hi, n) {
    const raw = (hi - lo) / n;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const f = raw / mag;
    const step = (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * mag;
    const a = Math.floor(lo / step) * step, b = Math.ceil(hi / step) * step;
    const ticks = [];
    for (let v = a; v <= b + step / 2; v += step) ticks.push(Math.round(v * 1e6) / 1e6);
    return { ticks, lo: a, hi: b };
  }

  const TIME_STEPS = [600, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 2 * 86400, 7 * 86400, 14 * 86400, 30 * 86400];
  function timeTicks(x0, x1) {
    const span = x1 - x0;
    const step = TIME_STEPS.find((s) => span / s <= 7) || TIME_STEPS[TIME_STEPS.length - 1];
    const off = -new Date(x0 * 1000).getTimezoneOffset() * 60;
    const out = [];
    for (let t = Math.ceil((x0 + off) / step) * step - off; t <= x1; t += step) out.push(t);
    return { ticks: out, step };
  }

  function nearestIdx(rows, t) {
    let lo = 0, hi = rows.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (rows[mid][0] < t) lo = mid + 1; else hi = mid;
    }
    if (lo > 0 && Math.abs(rows[lo - 1][0] - t) <= Math.abs(rows[lo][0] - t)) lo--;
    return lo;
  }

  let lastW = 0;

  function drawChart(hist, series) {
    const wrap = $('chart');
    wrap.replaceChildren();
    if (!series.length) {
      wrap.append(h('p', { class: 'muted', text: 'Aucun relevé sur cette période.' }));
      return;
    }
    const W = Math.max(300, wrap.clientWidth || 900);
    lastW = W;
    const H = W < 520 ? 240 : 300;
    const m = { l: 46, r: 18, t: 12, b: 28 };
    const pw = W - m.l - m.r, ph = H - m.t - m.b;
    const x0 = hist.since, x1 = hist.until;

    let lo = Infinity, hi = -Infinity;
    for (const s of series) for (const r of s.rows) { lo = Math.min(lo, r[2]); hi = Math.max(hi, r[3]); }
    const dataLo = lo, dataHi = hi;
    if (state.thr) for (const t of [hist.warn, hist.crit]) { hi = Math.max(hi, t); lo = Math.min(lo, t); }
    if (hi - lo < 4) { const mid = (hi + lo) / 2; lo = mid - 2; hi = mid + 2; }
    const pad = (hi - lo) * 0.06;
    const nt = niceTicks(lo - pad, hi + pad, 4);
    const px = (t) => m.l + ((t - x0) / (x1 - x0)) * pw;
    const py = (v) => m.t + ph - ((v - nt.lo) / (nt.hi - nt.lo)) * ph;

    const svg = sv('svg', {
      viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: 'img', tabindex: 0,
      'aria-label': 'Courbe de température de ' + hist.name + ', flèches gauche et droite pour parcourir les relevés',
    });

    // grille et axes
    for (const v of nt.ticks) {
      svg.append(sv('line', { class: 'grid', x1: m.l, x2: W - m.r, y1: py(v), y2: py(v) }));
      svg.append(sv('text', { x: m.l - 8, y: py(v) + 4, 'text-anchor': 'end' }, String(v).replace('.', ',')));
    }
    svg.append(sv('line', { class: 'axis', x1: m.l, x2: W - m.r, y1: m.t + ph, y2: m.t + ph }));
    const tt = timeTicks(x0, x1);
    const longRange = x1 - x0 > 36 * 3600;
    for (const t of tt.ticks) {
      const x = px(t);
      if (x < m.l + 14 || x > W - m.r - 14) continue;
      const d = new Date(t * 1000);
      let label;
      if (tt.step >= 86400) label = dayShort.format(d);
      else if (longRange) label = new Intl.DateTimeFormat('fr-FR', { weekday: 'short' }).format(d) + ' ' + hm(d);
      else label = hm(d);
      svg.append(sv('line', { class: 'axis', x1: x, x2: x, y1: m.t + ph, y2: m.t + ph + 4 }));
      svg.append(sv('text', { x, y: H - 8, 'text-anchor': 'middle' }, label));
    }

    // seuils (traits pointillés, texte en encre neutre)
    (state.thr ? [['Attention', hist.warn, 'var(--warn)'], ['Critique', hist.crit, 'var(--crit)']] : []).forEach(([name, v, color]) => {
      if (v < nt.lo || v > nt.hi) return;
      svg.append(sv('line', { class: 'thr', stroke: color, x1: m.l, x2: W - m.r, y1: py(v), y2: py(v) }));
      svg.append(sv('text', { class: 'lbl', x: m.l + 6, y: py(v) - 5 }, name + ' ' + v + ' °C'));
    });

    // séries
    const gap = Math.max(3 * hist.step, 180);
    const hc = hostColor(hist.host);
    const colorOf = () => hc;
    const dashOf = (i) => DASHES[i % 4];
    series.forEach((s, i) => {
      const segs = [];
      let cur = [];
      s.rows.forEach((r, j) => {
        if (j && r[0] - s.rows[j - 1][0] > gap) { segs.push(cur); cur = []; }
        cur.push(r);
      });
      segs.push(cur);
      for (const seg of segs) {
        if (seg.length === 1) {
          svg.append(sv('circle', { cx: px(seg[0][0]), cy: py(seg[0][1]), r: 2.5, fill: colorOf(i) }));
          continue;
        }
        let up = '', down = '', line = '';
        seg.forEach((r, j) => {
          line += (j ? 'L' : 'M') + px(r[0]).toFixed(1) + ' ' + py(r[1]).toFixed(1);
          up += (j ? 'L' : 'M') + px(r[0]).toFixed(1) + ' ' + py(r[3]).toFixed(1);
        });
        for (let j = seg.length - 1; j >= 0; j--) down += 'L' + px(seg[j][0]).toFixed(1) + ' ' + py(seg[j][2]).toFixed(1);
        svg.append(sv('path', { class: 'band', fill: colorOf(i), d: up + down + 'Z' }));
        svg.append(sv('path', { class: 'line', stroke: colorOf(i), 'stroke-dasharray': dashOf(i), d: line }));
      }
      const last = s.rows[s.rows.length - 1];
      svg.append(sv('circle', { class: 'ring', cx: px(last[0]), cy: py(last[1]), r: 4, fill: colorOf(i) }));
    });

    // calque d'interaction
    const cross = sv('line', { class: 'cross', y1: m.t, y2: m.t + ph, visibility: 'hidden' });
    svg.append(cross);
    const dots = series.map((s, i) => {
      const c = sv('circle', { class: 'ring', r: 4, fill: colorOf(i), visibility: 'hidden' });
      svg.append(c);
      return c;
    });
    const tip = h('div', { class: 'tip' });
    tip.hidden = true;
    const hit = sv('rect', { x: m.l, y: m.t, width: pw, height: ph, fill: 'transparent' });
    svg.append(hit);
    wrap.append(svg, tip);

    const primary = series[0].rows;
    let idx = null;
    const tol = hist.step * 1.5 + 90;

    function hide() {
      idx = null;
      cross.setAttribute('visibility', 'hidden');
      dots.forEach((c) => c.setAttribute('visibility', 'hidden'));
      tip.hidden = true;
    }
    function show(i) {
      idx = i;
      const t = primary[i][0];
      const x = px(t);
      cross.setAttribute('x1', x);
      cross.setAttribute('x2', x);
      cross.setAttribute('visibility', 'visible');
      tip.replaceChildren(h('div', { class: 't', text: fmtTime(t, true) }));
      series.forEach((s, k) => {
        const j = nearestIdx(s.rows, t);
        const r = s.rows[j];
        if (Math.abs(r[0] - t) > tol) { dots[k].setAttribute('visibility', 'hidden'); return; }
        dots[k].setAttribute('cx', px(r[0]));
        dots[k].setAttribute('cy', py(r[1]));
        dots[k].setAttribute('visibility', 'visible');
        const key = h('span', { class: 'sq' });
        key.style.setProperty('--hc', hc);
        tip.append(h('div', { class: 'r' }, key,
          h('span', { class: 'n', text: sensorLabel(s.sensor) }),
          h('span', { class: 'v', text: fmt(r[1]) + ' °C' })));
        if (r[4] > 1) tip.append(h('div', { class: 'mm', text: 'min ' + fmt(r[2]) + ' · max ' + fmt(r[3]) }));
      });
      tip.hidden = false;
      const w = tip.offsetWidth;
      tip.style.left = (x > W / 2 ? x - w - 12 : x + 12) + 'px';
      tip.style.top = m.t + 4 + 'px';
    }
    function fromEvent(ev) {
      const box = svg.getBoundingClientRect();
      const x = ((ev.clientX - box.left) / box.width) * W;
      const t = x0 + ((x - m.l) / pw) * (x1 - x0);
      return nearestIdx(primary, t);
    }
    hit.addEventListener('pointermove', (ev) => show(fromEvent(ev)));
    hit.addEventListener('pointerdown', (ev) => show(fromEvent(ev)));
    hit.addEventListener('pointerleave', hide);
    svg.addEventListener('blur', hide);
    svg.addEventListener('keydown', (ev) => {
      let next = null;
      if (ev.key === 'ArrowLeft') next = idx == null ? primary.length - 1 : Math.max(0, idx - 1);
      else if (ev.key === 'ArrowRight') next = idx == null ? primary.length - 1 : Math.min(primary.length - 1, idx + 1);
      else if (ev.key === 'Home') next = 0;
      else if (ev.key === 'End') next = primary.length - 1;
      else if (ev.key === 'Escape') { hide(); return; }
      if (next != null) { ev.preventDefault(); show(next); }
    });
  }

  function drawTable(series) {
    const box = $('table');
    box.replaceChildren();
    if (!series.length) {
      box.append(h('p', { class: 'muted', text: 'Aucun relevé sur cette période.' }));
      return;
    }
    const maps = series.map((s) => new Map(s.rows.map((r) => [r[0], r])));
    const times = [];
    const seen = new Set();
    for (const s of series) for (const r of s.rows) if (!seen.has(r[0])) { seen.add(r[0]); times.push(r[0]); }
    times.sort((a, b) => b - a);
    const withDate = state.range !== '1h' && state.range !== '6h';
    const head = h('tr', {}, h('th', { text: 'Heure' }), series.map((s) => h('th', { text: sensorLabel(s.sensor) + ' (moy., min–max)' })));
    const body = times.map((t) => h('tr', {}, h('td', { text: fmtTime(t, withDate) }),
      maps.map((mp) => {
        const r = mp.get(t);
        return h('td', { text: r ? fmt(r[1]) + ' (' + fmt(r[2]) + '–' + fmt(r[3]) + ')' : '—' });
      })));
    box.append(h('table', {}, h('thead', {}, head), h('tbody', {}, body)));
  }

  // ---------- cycle de rafraîchissement ----------
  async function refresh() {
    const seq = ++state.seqHosts;
    try {
      const d = await getJSON('/api/hosts?range=' + state.range);
      if (seq !== state.seqHosts) return;
      state.data = d;
      banner(false);
      renderSummary(d);
      renderCards(d);
      const now = new Date();
      $('updated').textContent = 'Mis à jour ' + hm(now) + ':' + pad2(now.getSeconds());
      if (window.LZ.overview) window.LZ.overview.load();
      await loadHistory();
    } catch (e) {
      if (seq !== state.seqHosts) return;
      $('cards').classList.remove('loading');
      $('chart').classList.remove('loading');
      banner(true, 'Connexion perdue avec Lézard' + (state.data ? ' — dernières données reçues à ' + hm(new Date(state.data.now * 1000)) + '.' : '.'));
    }
  }

  // le graphique suit la largeur de son conteneur
  if (window.ResizeObserver) {
    new ResizeObserver(() => {
      const w = $('chart').clientWidth;
      if (w && Math.abs(w - lastW) > 1 && state.hist && state.view === 'chart') renderDetail();
    }).observe($('chart'));
  }

  $('thr-toggle').addEventListener('click', () => {
    state.thr = !state.thr;
    store.set('lezard.thr', state.thr ? '1' : '0');
    renderDetail();
  });

  window.LZ = {
    h, sv, fmt, fmtTime, niceTicks, timeTicks, nearestIdx, stepText, hm, dayShort, getJSON, store, state,
    colorVar, rangeInfo, DASHES, sensorLabel, overview: null,
  };

  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
  setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);

  renderRanges();
  refresh();
})();
