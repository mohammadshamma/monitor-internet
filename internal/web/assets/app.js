'use strict';

/* Verdict presentation.
 *
 * These are STATUS colours, not categorical series colours: they encode state.
 * Every one ships with a glyph and a label so colour is never the only channel
 * — which also covers the one sub-floor pair in this palette (warning vs
 * serious, i.e. "degraded" vs "your own LAN").
 */
const STATE = {
  OK:           { label: 'Fine',              glyph: '●', varName: '--status-good',     blame: null },
  ICMP_DEPRIO:  { label: 'Fine (quiet hop)',  glyph: '●', varName: '--status-good',     blame: null },
  DEGRADED:     { label: 'Degraded',          glyph: '▲', varName: '--status-warning',  blame: null },
  LAN_FAULT:    { label: 'Your network',      glyph: '◆', varName: '--status-serious',  blame: 'you' },
  ISP_UPSTREAM: { label: 'Provider upstream', glyph: '■', varName: '--status-critical', blame: 'isp' },
  ISP_FAULT:    { label: 'Provider down',     glyph: '■', varName: '--status-critical', blame: 'isp' },
  UNKNOWN:      { label: 'Not monitored',     glyph: '○', varName: '--status-unknown',  blame: null },
};

// Only these appear in the legend; the two "fine" states share a colour and the
// distinction between them is a detail for the tooltip, not the legend.
const LEGEND_ORDER = ['OK', 'DEGRADED', 'LAN_FAULT', 'ISP_FAULT', 'UNKNOWN'];

const state = { range: '7d' };

function cssVar(name) {
  return getComputedStyle(document.body).getPropertyValue(name).trim();
}
function stateOf(v) { return STATE[v] || STATE.UNKNOWN; }
function colorOf(v) { return cssVar(stateOf(v).varName); }

function humanDuration(sec) {
  sec = Math.round(sec || 0);
  if (sec <= 0) return '0s';
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ' + (sec % 60) + 's';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ' + Math.floor((sec % 3600) / 60) + 'm';
  return Math.floor(sec / 86400) + 'd ' + Math.floor((sec % 86400) / 3600) + 'h';
}

function fmtTime(d) {
  return d.toLocaleString(undefined, {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

async function getJSON(url) {
  const res = await fetch(url, { cache: 'no-store' });
  if (!res.ok) throw new Error(url + ': ' + res.status);
  return res.json();
}

const svgNS = 'http://www.w3.org/2000/svg';
function el(name, attrs) {
  const n = document.createElementNS(svgNS, name);
  for (const k in attrs) n.setAttribute(k, attrs[k]);
  return n;
}

/* ---------------------------------------------------------------- live pill */

async function refreshStatus() {
  try {
    const s = await getJSON('/api/status');
    const meta = stateOf(s.stale ? 'UNKNOWN' : s.verdict);
    document.getElementById('live-icon').style.background = cssVar(meta.varName);
    document.getElementById('live-label').textContent =
      s.stale ? 'Collector not running' : meta.label;

    const t = document.getElementById('live-time');
    if (s.last_cycle && !s.last_cycle.startsWith('0001')) {
      const d = new Date(s.last_cycle);
      const ago = Math.round((Date.now() - d.getTime()) / 1000);
      t.textContent = 'last check ' + (ago < 60 ? ago + 's ago' : humanDuration(ago) + ' ago');
    } else {
      t.textContent = 'no data yet';
    }

    const topo = [];
    if (s.gateway) topo.push('router ' + s.gateway);
    if (s.isp_edge) topo.push('ISP edge ' + s.isp_edge);
    if (s.interface) topo.push('via ' + s.interface);
    document.getElementById('topology').textContent = topo.join('  ·  ') || '—';
  } catch (err) {
    document.getElementById('live-label').textContent = 'dashboard offline';
  }
}

/* ------------------------------------------------------------ summary/tiles */

function rangeLabel(r) {
  return { '24h': '24 hours', '7d': '7 days', '30d': '30 days', '90d': '90 days' }[r] || r;
}

async function refreshSummary() {
  const data = await getJSON('/api/summary?since=' + state.range);
  const s = data.summary;

  document.getElementById('hero-window').textContent = rangeLabel(state.range);
  document.getElementById('hero').textContent =
    s.monitored_seconds > 0 ? s.isp_availability_pct.toFixed(3) + '%' : '—';

  document.getElementById('t-coverage').textContent = s.coverage_pct.toFixed(1) + '%';
  document.getElementById('t-coverage-note').textContent =
    humanDuration(s.monitored_seconds) + ' of ' + humanDuration(s.window_seconds) + ' monitored';

  document.getElementById('t-isp').textContent = humanDuration(s.isp_downtime_seconds);
  document.getElementById('t-isp-note').textContent =
    'your own equipment: ' + humanDuration(s.lan_downtime_seconds);

  document.getElementById('t-outages').textContent = s.isp_outage_count;
  document.getElementById('t-outages-note').textContent =
    s.outage_count + ' total, incl. your own network';

  document.getElementById('t-longest').textContent =
    s.longest_outage_seconds > 0 ? humanDuration(s.longest_outage_seconds) : '—';
  document.getElementById('t-longest-note').textContent =
    s.longest_outage_class ? stateOf(s.longest_outage_class).label : 'no outages recorded';

  renderWarnings(s, data.links);
  renderHours(data.hour_isp || []);
}

/* Surfacing the things that would otherwise quietly invalidate the numbers. */
function renderWarnings(s, links) {
  const box = document.getElementById('warnings');
  box.innerHTML = '';

  if (s.monitored_seconds === 0) {
    box.appendChild(warnEl(
      'No data in this window.',
      'The collector has not recorded any cycles here yet.'));
    return;
  }
  if (s.coverage_pct < 95 && s.window_seconds >= 600) {
    box.appendChild(warnEl(
      'Coverage is ' + s.coverage_pct.toFixed(1) + '%.',
      'The monitor was not running for part of this window, so these figures ' +
      'describe only the time it was watching — they are not a claim about the rest.'));
  }
  if (links && links.length > 1) {
    const names = links.map(l => (l.ssid ? l.interface + ' / ' + l.ssid : l.interface)).join(', ');
    box.appendChild(warnEl(
      'The network link changed during this window.',
      'Seen on: ' + names + '. Comparisons across the whole period are not like-for-like.'));
  }
}

function warnEl(strong, rest) {
  const d = document.createElement('div');
  d.className = 'warn';
  const b = document.createElement('strong');
  b.textContent = strong + ' ';
  d.appendChild(b);
  d.appendChild(document.createTextNode(rest));
  return d;
}

/* -------------------------------------------------------------- the legend */

function renderLegend() {
  const box = document.getElementById('legend');
  box.innerHTML = '';
  for (const key of LEGEND_ORDER) {
    const meta = STATE[key];
    const item = document.createElement('span');
    item.className = 'legend-item';

    const sw = document.createElement('span');
    sw.className = 'legend-swatch';
    sw.style.background = cssVar(meta.varName);

    const glyph = document.createElement('span');
    glyph.className = 'legend-glyph';
    glyph.textContent = meta.glyph;

    const label = document.createElement('span');
    label.textContent = meta.label;

    item.append(sw, glyph, label);
    box.appendChild(item);
  }
}

/* ------------------------------------------------------------ timeline strip */

async function refreshTimeline() {
  const svg = document.getElementById('strip');
  const width = svg.clientWidth || 900;
  const height = 46;
  const buckets = Math.max(40, Math.min(600, Math.floor(width / 4)));

  const data = await getJSON(
    '/api/timeline?since=' + state.range + '&buckets=' + buckets);

  svg.setAttribute('viewBox', '0 0 ' + width + ' ' + height);
  svg.innerHTML = '';
  if (!data || !data.length) return;

  // Merge neighbouring buckets in the same state into runs, then leave a 2px
  // surface gap between runs. Gapping every bucket would shred a continuous
  // band into confetti; gapping between runs marks the actual state changes.
  const runs = [];
  for (let i = 0; i < data.length; i++) {
    const v = data[i].cycles > 0 ? data[i].dominant : 'UNKNOWN';
    const last = runs[runs.length - 1];
    if (last && last.verdict === v) {
      last.end = i;
      last.cycles += data[i].cycles;
    } else {
      runs.push({ verdict: v, start: i, end: i, cycles: data[i].cycles });
    }
  }

  const slot = width / data.length;
  const tip = document.getElementById('strip-tip');

  for (const run of runs) {
    const x = run.start * slot;
    const raw = (run.end - run.start + 1) * slot;
    const w = Math.max(1.5, raw - (runs.length > 1 ? 2 : 0));

    const rect = el('rect', {
      x: x.toFixed(2), y: 0, width: w.toFixed(2), height: height,
      rx: Math.min(3, w / 2).toFixed(2),
      fill: colorOf(run.verdict),
    });

    const from = new Date(data[run.start].start);
    const toIdx = Math.min(run.end + 1, data.length - 1);
    const to = new Date(data[toIdx].start);
    const meta = stateOf(run.verdict);

    // Hover layer is default, not optional.
    rect.addEventListener('mouseenter', () => {
      tip.hidden = false;
      tip.innerHTML = '';
      const head = document.createElement('div');
      head.className = 'tt-head';
      const dot = document.createElement('span');
      dot.className = 'state-dot';
      dot.style.background = colorOf(run.verdict);
      head.append(dot, document.createTextNode(meta.glyph + ' ' + meta.label));
      const sub = document.createElement('div');
      sub.className = 'tt-sub';
      sub.textContent = fmtTime(from) + ' → ' + fmtTime(to);
      tip.append(head, sub);
    });
    rect.addEventListener('mousemove', (ev) => {
      const box = svg.parentElement.getBoundingClientRect();
      const left = Math.min(
        Math.max(0, ev.clientX - box.left - tip.offsetWidth / 2),
        box.width - tip.offsetWidth);
      tip.style.left = left + 'px';
      tip.style.top = (height + 8) + 'px';
    });
    rect.addEventListener('mouseleave', () => { tip.hidden = true; });

    svg.appendChild(rect);
  }

  const axis = document.getElementById('strip-axis');
  axis.innerHTML = '';
  const a = document.createElement('span');
  a.textContent = fmtTime(new Date(data[0].start));
  const b = document.createElement('span');
  b.textContent = fmtTime(new Date(data[data.length - 1].start));
  axis.append(a, b);
}

/* ------------------------------------------------- downtime by hour of day */

function renderHours(hist) {
  const card = document.getElementById('hours-card');
  const total = hist.reduce((a, b) => a + b, 0);
  if (!total) { card.hidden = true; return; }
  card.hidden = false;

  const svg = document.getElementById('hours');
  const width = svg.clientWidth || 900;
  const height = 190;
  // padT gives the topmost tick label room; at 10 it sat flush against the
  // top edge and read as clipped.
  const padL = 56, padR = 8, padT = 18, padB = 26;
  const plotW = width - padL - padR;
  const plotH = height - padT - padB;

  svg.setAttribute('viewBox', '0 0 ' + width + ' ' + height);
  svg.innerHTML = '';

  const peak = Math.max(...hist);
  const niceMax = peak;
  const slot = plotW / 24;
  const barW = Math.min(24, slot - 4);
  const tip = document.getElementById('hours-tip');

  // Recessive gridlines and a single baseline.
  for (const frac of [0, 0.5, 1]) {
    const y = padT + plotH - frac * plotH;
    svg.appendChild(el('line', {
      x1: padL, x2: width - padR, y1: y.toFixed(1), y2: y.toFixed(1),
      stroke: frac === 0 ? cssVar('--baseline') : cssVar('--gridline'),
      'stroke-width': 1,
    }));
    const t = el('text', {
      x: padL - 8, y: (y + 4).toFixed(1),
      'text-anchor': 'end', fill: cssVar('--text-muted'),
      'font-size': '11', 'font-family': 'system-ui, sans-serif',
    });
    t.textContent = humanDuration(niceMax * frac);
    svg.appendChild(t);
  }

  for (let h = 0; h < 24; h++) {
    if (hist[h] > 0) {
      const barH = Math.max(2, (hist[h] / niceMax) * plotH);
      const x = padL + h * slot + (slot - barW) / 2;
      const y = padT + plotH - barH;
      const r = Math.min(4, barW / 2, barH);

      // Rounded data-end, square at the baseline.
      const path = el('path', {
        d: `M${x},${y + barH} L${x},${y + r} Q${x},${y} ${x + r},${y} ` +
           `L${x + barW - r},${y} Q${x + barW},${y} ${x + barW},${y + r} ` +
           `L${x + barW},${y + barH} Z`,
        fill: cssVar('--status-critical'),
      });

      path.addEventListener('mouseenter', () => {
        tip.hidden = false;
        tip.innerHTML = '';
        const head = document.createElement('div');
        head.className = 'tt-head';
        head.textContent = String(h).padStart(2, '0') + ':00';
        const sub = document.createElement('div');
        sub.className = 'tt-sub';
        sub.textContent = humanDuration(hist[h]) + ' provider downtime';
        tip.append(head, sub);
        tip.style.left = Math.min(x, width - 160) + 'px';
        tip.style.top = Math.max(0, y - 44) + 'px';
      });
      path.addEventListener('mouseleave', () => { tip.hidden = true; });
      svg.appendChild(path);
    }

    if (h % 3 === 0) {
      const t = el('text', {
        x: (padL + h * slot + slot / 2).toFixed(1), y: height - 8,
        'text-anchor': 'middle', fill: cssVar('--text-muted'),
        'font-size': '11', 'font-family': 'system-ui, sans-serif',
      });
      t.textContent = String(h).padStart(2, '0');
      svg.appendChild(t);
    }
  }
}

/* --------------------------------------------------- latency by target */

// Categorical slots assigned in fixed order and never cycled. Colour follows
// the target, so changing the range never repaints a series.
const SERIES_VARS = ['--series-1', '--series-2', '--series-3', '--series-4', '--series-5'];

function fmtMs(v) {
  if (v === null || v === undefined) return '—';
  return (v < 10 ? v.toFixed(2) : v.toFixed(1)) + ' ms';
}

async function refreshLatency() {
  const svg = document.getElementById('latency');
  const width = svg.clientWidth || 900;
  const height = 240;
  const buckets = Math.max(30, Math.min(300, Math.floor(width / 6)));

  const series = await getJSON(
    '/api/targets?since=' + state.range + '&buckets=' + buckets);

  const note = document.getElementById('latency-note');
  svg.innerHTML = '';
  renderTargetTable(series);

  const withData = series.filter(s => s.samples > 0);
  if (!withData.length) {
    note.hidden = false;
    note.textContent = 'No latency samples recorded in this window yet.';
    document.getElementById('latency-legend').innerHTML = '';
    return;
  }

  // Be explicit when the query strided over the data rather than reading it
  // all — a thinned series must not be mistaken for the full record.
  if (withData[0].truncated) {
    note.hidden = false;
    note.textContent =
      'Long window: the chart samples one cycle every ' + withData[0].stride_seconds +
      's rather than reading every probe. Percentiles below are computed from the same sample.';
  } else {
    note.hidden = true;
  }

  // padT leaves room for the unit caption above the topmost tick label, which
  // would otherwise collide with it.
  const padL = 52, padR = 12, padT = 26, padB = 24;
  const plotW = width - padL - padR;
  const plotH = height - padT - padB;
  svg.setAttribute('viewBox', '0 0 ' + width + ' ' + height);

  let peak = 0;
  for (const s of withData) {
    for (const p of s.points) if (p.avg_rtt_ms !== null && p.avg_rtt_ms > peak) peak = p.avg_rtt_ms;
  }
  if (peak <= 0) peak = 1;
  const yMax = peak * 1.15;
  const n = withData[0].points.length;
  const xOf = i => padL + (n <= 1 ? 0 : (i / (n - 1)) * plotW);
  const yOf = v => padT + plotH - (v / yMax) * plotH;

  // Recessive gridlines; one axis only.
  for (const frac of [0, 0.25, 0.5, 0.75, 1]) {
    const y = padT + plotH - frac * plotH;
    svg.appendChild(el('line', {
      x1: padL, x2: width - padR, y1: y.toFixed(1), y2: y.toFixed(1),
      stroke: frac === 0 ? cssVar('--baseline') : cssVar('--gridline'), 'stroke-width': 1,
    }));
    const t = el('text', {
      x: padL - 8, y: (y + 4).toFixed(1), 'text-anchor': 'end',
      fill: cssVar('--text-muted'), 'font-size': '11',
      'font-family': 'system-ui, sans-serif',
    });
    t.textContent = (yMax * frac).toFixed(yMax < 20 ? 1 : 0);
    svg.appendChild(t);
  }
  const unit = el('text', {
    x: padL - 8, y: padT - 12, 'text-anchor': 'end',
    fill: cssVar('--text-muted'), 'font-size': '10.5',
    'font-family': 'system-ui, sans-serif',
  });
  unit.textContent = 'ms';
  svg.appendChild(unit);

  // One path per target. Gaps (unanswered probes) break the line rather than
  // being interpolated across — a straight line through an outage would be a
  // lie about what was measured.
  withData.forEach((s, si) => {
    const color = cssVar(SERIES_VARS[si % SERIES_VARS.length]);
    let d = '', pen = false;
    s.points.forEach((p, i) => {
      if (p.avg_rtt_ms === null) { pen = false; return; }
      const cmd = pen ? 'L' : 'M';
      d += cmd + xOf(i).toFixed(1) + ',' + yOf(p.avg_rtt_ms).toFixed(1) + ' ';
      pen = true;
    });
    if (d) svg.appendChild(el('path', { d: d.trim(), class: 'series-line', stroke: color }));
  });

  renderLatencyLegend(withData);
  attachCrosshair(svg, withData, { width, height, padL, padT, plotW, plotH, xOf, yOf, n });

  const axis = document.getElementById('latency-axis');
  axis.innerHTML = '';
  const a = document.createElement('span');
  a.textContent = fmtTime(new Date(withData[0].points[0].start));
  const b = document.createElement('span');
  b.textContent = fmtTime(new Date(withData[0].points[n - 1].start));
  axis.append(a, b);
}

function renderLatencyLegend(series) {
  const box = document.getElementById('latency-legend');
  box.innerHTML = '';
  series.forEach((s, si) => {
    const item = document.createElement('span');
    item.className = 'legend-item';
    const sw = document.createElement('span');
    sw.className = 'legend-swatch';
    sw.style.background = cssVar(SERIES_VARS[si % SERIES_VARS.length]);
    const label = document.createElement('span');
    label.textContent = s.name + ' (' + s.addr + ')';
    item.append(sw, label);
    box.appendChild(item);
  });
}

// Crosshair + tooltip is the default interaction for a line chart.
function attachCrosshair(svg, series, geo) {
  const tip = document.getElementById('latency-tip');
  const line = el('line', {
    class: 'crosshair-line', y1: geo.padT, y2: geo.padT + geo.plotH,
    x1: 0, x2: 0, opacity: 0,
  });
  svg.appendChild(line);

  const dots = series.map((s, si) => {
    const c = el('circle', {
      class: 'crosshair-dot', r: 4, opacity: 0,
      fill: cssVar(SERIES_VARS[si % SERIES_VARS.length]),
    });
    svg.appendChild(c);
    return c;
  });

  // Hit area spans the whole plot, so the target is far bigger than the marks.
  const hit = el('rect', {
    x: geo.padL, y: geo.padT, width: geo.plotW, height: geo.plotH,
    fill: 'transparent', style: 'cursor:crosshair',
  });
  svg.appendChild(hit);

  hit.addEventListener('mousemove', (ev) => {
    const box = svg.getBoundingClientRect();
    const rel = (ev.clientX - box.left) * (geo.width / box.width);
    let idx = Math.round(((rel - geo.padL) / geo.plotW) * (geo.n - 1));
    idx = Math.max(0, Math.min(geo.n - 1, idx));

    const x = geo.xOf(idx);
    line.setAttribute('x1', x); line.setAttribute('x2', x);
    line.setAttribute('opacity', 1);

    tip.hidden = false;
    tip.innerHTML = '';
    const head = document.createElement('div');
    head.className = 'tt-head';
    head.textContent = fmtTime(new Date(series[0].points[idx].start));
    tip.appendChild(head);

    series.forEach((s, si) => {
      const p = s.points[idx];
      const row = document.createElement('div');
      row.className = 'tt-row';
      const name = document.createElement('span');
      name.className = 'tt-name';
      const sw = document.createElement('span');
      sw.className = 'tt-swatch';
      sw.style.background = cssVar(SERIES_VARS[si % SERIES_VARS.length]);
      name.append(sw, document.createTextNode(s.name));
      const val = document.createElement('span');
      val.className = 'tt-val';
      val.textContent = p.avg_rtt_ms === null
        ? 'no reply'
        : fmtMs(p.avg_rtt_ms) + (p.loss_pct > 0 ? '  ·  ' + p.loss_pct.toFixed(0) + '% loss' : '');
      row.append(name, val);
      tip.appendChild(row);

      if (p.avg_rtt_ms === null) {
        dots[si].setAttribute('opacity', 0);
      } else {
        dots[si].setAttribute('cx', x);
        dots[si].setAttribute('cy', geo.yOf(p.avg_rtt_ms));
        dots[si].setAttribute('opacity', 1);
      }
    });

    const wrap = svg.parentElement.getBoundingClientRect();
    const left = Math.min(
      Math.max(0, ev.clientX - wrap.left + 14),
      Math.max(0, wrap.width - tip.offsetWidth - 4));
    tip.style.left = left + 'px';
    tip.style.top = '8px';
  });

  hit.addEventListener('mouseleave', () => {
    tip.hidden = true;
    line.setAttribute('opacity', 0);
    for (const d of dots) d.setAttribute('opacity', 0);
  });
}

// The table view — also the relief for the light-mode contrast warning on
// three of the five categorical slots.
function renderTargetTable(series) {
  const body = document.getElementById('target-body');
  body.innerHTML = '';
  series.forEach((s, si) => {
    const tr = document.createElement('tr');

    const name = document.createElement('td');
    const cell = document.createElement('span');
    cell.className = 'state-cell';
    const dot = document.createElement('span');
    dot.className = 'state-dot';
    dot.style.background = cssVar(SERIES_VARS[si % SERIES_VARS.length]);
    cell.append(dot, document.createTextNode(s.name + ' (' + s.addr + ')'));
    name.appendChild(cell);

    const cells = [
      s.tier,
      fmtMs(s.min_rtt_ms),
      fmtMs(s.avg_rtt_ms),
      fmtMs(s.p95_rtt_ms),
      fmtMs(s.max_rtt_ms),
      s.samples > 0 ? s.loss_pct.toFixed(1) + '%' : '—',
    ].map(v => {
      const td = document.createElement('td');
      td.textContent = v;
      return td;
    });

    tr.append(name, ...cells);
    body.appendChild(tr);
  });
}

/* -------------------------------------------------------------- outage table */

async function refreshOutages() {
  const rows = await getJSON('/api/outages?since=' + state.range);
  const body = document.getElementById('outages-body');
  const empty = document.getElementById('outages-empty');
  body.innerHTML = '';

  if (!rows.length) {
    empty.hidden = false;
    document.getElementById('outages').hidden = true;
    return;
  }
  empty.hidden = true;
  document.getElementById('outages').hidden = false;

  for (const o of rows.slice(0, 200)) {
    const meta = stateOf(o.class);
    const tr = document.createElement('tr');

    const started = document.createElement('td');
    started.textContent = fmtTime(new Date(o.started));

    const dur = document.createElement('td');
    dur.textContent = humanDuration(o.duration_seconds) + (o.ongoing ? ' (ongoing)' : '');

    const st = document.createElement('td');
    const cell = document.createElement('span');
    cell.className = 'state-cell';
    const dot = document.createElement('span');
    dot.className = 'state-dot';
    dot.style.background = cssVar(meta.varName);
    cell.append(dot, document.createTextNode(meta.glyph + ' ' + meta.label));
    st.appendChild(cell);

    const blame = document.createElement('td');
    blame.textContent = o.blames_isp ? 'Provider' : 'You';
    if (o.blames_isp) blame.className = 'blame-isp';

    tr.append(started, dur, st, blame);
    body.appendChild(tr);
  }
}

/* --------------------------------------------------------------- wiring up */

async function refreshAll() {
  try {
    await Promise.all([refreshSummary(), refreshTimeline(), refreshLatency(), refreshOutages()]);
  } catch (err) {
    console.error(err);
  }
}

function initFilters() {
  for (const btn of document.querySelectorAll('.range')) {
    btn.addEventListener('click', () => {
      state.range = btn.dataset.range;
      for (const b of document.querySelectorAll('.range')) {
        b.classList.toggle('is-selected', b === btn);
        b.setAttribute('aria-pressed', String(b === btn));
      }
      refreshAll();
    });
  }
}

let resizeTimer;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => { refreshTimeline(); refreshLatency(); refreshSummary(); }, 200);
});

renderLegend();
initFilters();
refreshStatus();
refreshAll();
setInterval(refreshStatus, 5000);
setInterval(refreshAll, 60000);
