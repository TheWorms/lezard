(() => {
  'use strict';
  // Graphique « toutes les machines » : une courbe par machine, légende à bascules,
  // infobulle qui liste toutes les machines à l'instant survolé.
  const L = window.LZ;
  if (!L) return;
  const { h, sv, fmt, fmtTime, niceTicks, timeTicks, nearestIdx, stepText, hm, dayShort, getJSON, store, state, colorVar, rangeInfo, sensorLabel } = L;
  const $ = (id) => document.getElementById(id);

  let data = null;
  let seq = 0;
  let view = 'chart';
  let lastW = 0;
  let secondary = store.get('lezard.ov.sec') === '1';
  let hidden = new Set();
  try { hidden = new Set(JSON.parse(store.get('lezard.ov.hidden') || '[]')); } catch (e) { hidden = new Set(); }
  const saveHidden = () => store.set('lezard.ov.hidden', JSON.stringify([...hidden]));

  function buildSeries() {
    const out = [];
    for (const host of data.hosts) {
      const sensors = host.sensors.filter((s) => s.rows.length);
      sensors.forEach((s, i) => {
        if (i > 0 && !secondary) return;
        out.push({
          host: host.host, name: host.name, color: colorVar(host.color),
          label: host.name + (i > 0 ? ' · ' + sensorLabel(s.sensor) : ''),
          dash: i > 0 ? '7 4' : '', rows: s.rows,
        });
      });
    }
    return out;
  }

  function lastValue(host) {
    const x = data.hosts.find((e) => e.host === host);
    const s = x && x.sensors.find((e) => e.rows.length);
    return s ? s.rows[s.rows.length - 1][1] : null;
  }

  function renderLegend() {
    const box = $('ov-legend');
    box.replaceChildren();
    for (const host of data.hosts) {
      if (!host.sensors.some((s) => s.rows.length)) continue;
      const on = !hidden.has(host.host);
      const sw = h('span', { class: 'sw', 'aria-hidden': 'true' });
      sw.style.setProperty('--hc', colorVar(host.color));
      const v = lastValue(host.host);
      const b = h('button', { class: 'lchip', type: 'button', 'aria-pressed': String(on), title: on ? 'Masquer ' + host.name : 'Afficher ' + host.name },
        sw, host.name, v == null ? null : h('span', { class: 'val', text: fmt(v) + ' °C' }));
      b.addEventListener('click', () => {
        if (hidden.has(host.host)) hidden.delete(host.host); else hidden.add(host.host);
        saveHidden();
        render();
        const again = $('ov-legend').querySelectorAll('.lchip');
        for (const c of again) if (c.title.endsWith(host.name)) { c.focus({ preventScroll: true }); break; }
      });
      b.addEventListener('pointerenter', () => setFocus(host.host));
      b.addEventListener('pointerleave', () => setFocus(null));
      b.addEventListener('focus', () => setFocus(host.host));
      b.addEventListener('blur', () => setFocus(null));
      box.append(b);
    }
    const names = data.hosts.filter((x) => x.sensors.some((s) => s.rows.length)).map((x) => x.host);
    const all = h('button', { class: 'lchip all', type: 'button', 'aria-pressed': String(hidden.size === 0), text: 'Voir tout' });
    all.addEventListener('click', () => { hidden.clear(); saveHidden(); render(); });
    const none = h('button', { class: 'lchip all', type: 'button', 'aria-pressed': String(hidden.size >= names.length), text: 'Aucune' });
    none.addEventListener('click', () => { names.forEach((n) => hidden.add(n)); saveHidden(); render(); });
    box.prepend(all, none);
  }

  function setFocus(host) {
    const svg = $('ov-chart').querySelector('svg');
    if (!svg) return;
    svg.classList.toggle('focus', !!host);
    svg.querySelectorAll('g.ser').forEach((g) => g.classList.toggle('on', g.dataset.host === host));
  }

  function render() {
    if (!data) return;
    const panel = $('overview');
    const series = buildSeries();
    panel.hidden = !data.hosts.length;
    if (panel.hidden) return;
    $('ov-title').textContent = 'Toutes les machines — ' + rangeInfo()[2];
    const hasSec = data.hosts.some((x) => x.sensors.filter((s) => s.rows.length).length > 1);
    const sb = $('ov-sec');
    sb.hidden = !hasSec;
    sb.setAttribute('aria-pressed', String(secondary));
    const asTable = view === 'table';
    const vb = $('ov-view');
    vb.setAttribute('aria-pressed', String(asTable));
    vb.textContent = asTable ? 'Vue graphique' : 'Vue tableau';
    $('ov-chart').hidden = asTable;
    $('ov-table').hidden = !asTable;
    $('ov-chart').classList.remove('loading');
    renderLegend();
    const shown = series.filter((s) => !hidden.has(s.host));
    if (asTable) drawTable(shown); else drawChart(shown);
    $('ov-foot').textContent = 'Chaque point est la moyenne sur ' + stepText(data.step) +
      '. Survole le graphique pour comparer toutes les machines à un instant ; clique sur un nom pour masquer ou afficher sa courbe. ' +
      'Plusieurs teintes se ressemblent : la légende et l’infobulle donnent toujours les noms.';
  }

  function drawChart(series) {
    const wrap = $('ov-chart');
    wrap.replaceChildren();
    if (!series.length) {
      wrap.append(h('p', { class: 'muted', text: 'Aucune machine affichée : clique sur un nom dans la légende.' }));
      return;
    }
    const W = Math.max(300, wrap.clientWidth || 900);
    lastW = W;
    const H = W < 520 ? 300 : 340;
    const m = { l: 46, r: 18, t: 12, b: 28 };
    const pw = W - m.l - m.r, ph = H - m.t - m.b;
    const x0 = data.since, x1 = data.until;

    let lo = Infinity, hi = -Infinity;
    for (const s of series) for (const r of s.rows) { lo = Math.min(lo, r[1]); hi = Math.max(hi, r[1]); }
    if (hi - lo < 4) { const mid = (hi + lo) / 2; lo = mid - 2; hi = mid + 2; }
    const pad = (hi - lo) * 0.06;
    const nt = niceTicks(lo - pad, hi + pad, 5);
    const px = (t) => m.l + ((t - x0) / (x1 - x0)) * pw;
    const py = (v) => m.t + ph - ((v - nt.lo) / (nt.hi - nt.lo)) * ph;

    const svg = sv('svg', {
      viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: 'img', tabindex: 0,
      'aria-label': 'Courbes de température de toutes les machines, flèches gauche et droite pour parcourir les relevés',
    });
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

    const gap = Math.max(3 * data.step, 180);
    series.forEach((s) => {
      const g = sv('g', { class: 'ser', 'data-host': s.host });
      g.style.setProperty('--hc', s.color);
      const segs = [];
      let cur = [];
      s.rows.forEach((r, j) => {
        if (j && r[0] - s.rows[j - 1][0] > gap) { segs.push(cur); cur = []; }
        cur.push(r);
      });
      segs.push(cur);
      for (const seg of segs) {
        if (seg.length === 1) { g.append(sv('circle', { cx: px(seg[0][0]), cy: py(seg[0][1]), r: 2.5 })); continue; }
        let d = '';
        seg.forEach((r, j) => { d += (j ? 'L' : 'M') + px(r[0]).toFixed(1) + ' ' + py(r[1]).toFixed(1); });
        const attrs = { class: 'line', d };
        if (s.dash) attrs['stroke-dasharray'] = s.dash;
        g.append(sv('path', attrs));
      }
      const last = s.rows[s.rows.length - 1];
      g.append(sv('circle', { class: 'ring', cx: px(last[0]), cy: py(last[1]), r: 4 }));
      svg.append(g);
    });

    const cross = sv('line', { class: 'cross', y1: m.t, y2: m.t + ph, visibility: 'hidden' });
    svg.append(cross);
    const dots = series.map((s) => {
      const g = sv('g', { class: 'ser-dot', visibility: 'hidden' });
      g.style.setProperty('--hc', s.color);
      g.append(sv('circle', { class: 'ring', r: 4.5 }));
      svg.append(g);
      return g;
    });
    const tip = h('div', { class: 'tip' });
    tip.hidden = true;
    const hit = sv('rect', { x: m.l, y: m.t, width: pw, height: ph, fill: 'transparent' });
    svg.append(hit);
    wrap.append(svg, tip);

    const times = [...new Set(series.flatMap((s) => s.rows.map((r) => r[0])))].sort((a, b) => a - b);
    const tol = data.step * 1.5 + 90;
    let idx = null;

    function hide() {
      idx = null;
      cross.setAttribute('visibility', 'hidden');
      dots.forEach((d) => d.setAttribute('visibility', 'hidden'));
      tip.hidden = true;
      setFocus(null);
    }
    function show(i, cy) {
      idx = i;
      const t = times[i];
      const x = px(t);
      cross.setAttribute('x1', x);
      cross.setAttribute('x2', x);
      cross.setAttribute('visibility', 'visible');
      const entries = [];
      series.forEach((s, k) => {
        const r = s.rows[nearestIdx(s.rows, t)];
        if (Math.abs(r[0] - t) > tol) { dots[k].setAttribute('visibility', 'hidden'); return; }
        dots[k].setAttribute('transform', `translate(${px(r[0])} ${py(r[1])})`);
        dots[k].setAttribute('visibility', 'visible');
        entries.push({ s, v: r[1] });
      });
      entries.sort((a, b) => b.v - a.v);
      let hot = null;
      if (cy != null && entries.length) {
        hot = entries.reduce((best, e) => (Math.abs(py(e.v) - cy) < Math.abs(py(best.v) - cy) ? e : best), entries[0]);
      }
      tip.replaceChildren(h('div', { class: 't', text: fmtTime(t, true) }));
      for (const e of entries) {
        const sq = h('span', { class: 'sq' });
        sq.style.setProperty('--hc', e.s.color);
        tip.append(h('div', { class: 'r' + (e === hot ? ' hot' : '') }, sq,
          h('span', { class: 'n', text: e.s.label }),
          h('span', { class: 'v', text: fmt(e.v) + ' °C' })));
      }
      tip.hidden = false;
      setFocus(hot ? hot.s.host : null);
      const w = tip.offsetWidth, th = tip.offsetHeight;
      tip.style.left = Math.max(4, x > W / 2 ? x - w - 12 : x + 12) + 'px';
      tip.style.top = Math.max(0, Math.min(m.t + 4, H - th - 4)) + 'px';
    }
    function fromEvent(ev) {
      const box = svg.getBoundingClientRect();
      const x = ((ev.clientX - box.left) / box.width) * W;
      const cy = ((ev.clientY - box.top) / box.height) * H;
      const t = x0 + ((x - m.l) / pw) * (x1 - x0);
      let lo2 = 0, hi2 = times.length - 1;
      while (lo2 < hi2) { const mid = (lo2 + hi2) >> 1; if (times[mid] < t) lo2 = mid + 1; else hi2 = mid; }
      if (lo2 > 0 && Math.abs(times[lo2 - 1] - t) <= Math.abs(times[lo2] - t)) lo2--;
      return [lo2, cy];
    }
    hit.addEventListener('pointermove', (ev) => show(...fromEvent(ev)));
    hit.addEventListener('pointerdown', (ev) => show(...fromEvent(ev)));
    hit.addEventListener('pointerleave', hide);
    svg.addEventListener('blur', hide);
    svg.addEventListener('keydown', (ev) => {
      const last = times.length - 1;
      let next = null;
      if (ev.key === 'ArrowLeft') next = idx == null ? last : Math.max(0, idx - 1);
      else if (ev.key === 'ArrowRight') next = idx == null ? last : Math.min(last, idx + 1);
      else if (ev.key === 'Home') next = 0;
      else if (ev.key === 'End') next = last;
      else if (ev.key === 'Escape') { hide(); return; }
      if (next != null) { ev.preventDefault(); show(next, null); }
    });
  }

  function drawTable(series) {
    const box = $('ov-table');
    box.replaceChildren();
    if (!series.length) {
      box.append(h('p', { class: 'muted', text: 'Aucune machine affichée.' }));
      return;
    }
    const maps = series.map((s) => new Map(s.rows.map((r) => [r[0], r])));
    const times = [...new Set(series.flatMap((s) => s.rows.map((r) => r[0])))].sort((a, b) => b - a);
    const withDate = state.range !== '1h' && state.range !== '6h';
    const head = h('tr', {}, h('th', { text: 'Heure' }), series.map((s) => h('th', { text: s.label + ' (°C)' })));
    const body = times.map((t) => h('tr', {}, h('td', { text: fmtTime(t, withDate) }),
      maps.map((mp) => { const r = mp.get(t); return h('td', { text: r ? fmt(r[1]) : '—' }); })));
    box.append(h('table', {}, h('thead', {}, head), h('tbody', {}, body)));
  }

  async function load() {
    const my = ++seq;
    try {
      const d = await getJSON('/api/history/all?range=' + state.range);
      if (my !== seq) return;
      data = d;
      render();
    } catch (e) {
      if (my !== seq) return;
      $('ov-chart').classList.remove('loading');
    }
  }

  $('ov-view').addEventListener('click', () => { view = view === 'chart' ? 'table' : 'chart'; render(); });
  $('ov-sec').addEventListener('click', () => { secondary = !secondary; store.set('lezard.ov.sec', secondary ? '1' : '0'); render(); });
  if (window.ResizeObserver) {
    new ResizeObserver(() => {
      const w = $('ov-chart').clientWidth;
      if (w && Math.abs(w - lastW) > 1 && data && view === 'chart') render();
    }).observe($('ov-chart'));
  }

  L.overview = { load };
  if (state.data) load();
})();
