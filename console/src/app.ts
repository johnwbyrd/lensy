// LENS Detect operator console (prototype).
//
// One EventSource connection carries a snapshot and then every committed
// event. The console never derives state on its own: it shows what the event
// core has committed, reports back the moment an alert was painted, and makes
// silence visible (stale connection banner, stale feeds, escalations).

type Timeline = Record<string, string>;
interface Alert {
  id: string; trace_id: string; agency: string; camera: string; hazard: string; confidence: number;
  model_version: string; status: string; probe: boolean; suppressed: boolean; escalated: boolean;
  evidence_url?: string; evidence_sha256?: string; bbox: number[]; stationary_seconds: number;
  operator?: string; note?: string; timeline: Timeline; latency_ms: Record<string, number>;
}
interface Feed { camera: string; agency: string; status: string; last_heartbeat: string; last_frame_ts: string; fps: number; frozen: boolean; scene_change: boolean; source?: string; seconds: Record<string, number>; }
interface KillSwitch { agency: string; active: boolean; operator?: string; reason?: string; since: string; }
interface Pct { n: number; p50_ms: number; p95_ms: number; p99_ms: number; max_ms: number; }
interface Metrics { uptime_s: number; last_seq: number; event_counts: Record<string, number>; alerts_by_status: Record<string, number>; feeds_by_status: Record<string, number>; effective_coverage: number; lost_coverage_seconds: Record<string, number>; latency: Record<string, Pct>; probes: { total: number; last_ok: string; last_ok_age_s: number }; kill_switches: KillSwitch[] | null; }
interface Ev { event_id: string; seq: number; trace_id?: string; type: string; ts: string; agency?: string; resource?: string; actor?: string; payload?: any; }
interface Frame { kind: string; event?: Ev; alert?: Alert; feed?: Feed; alerts?: Alert[]; feeds?: Feed[]; kill_switches?: KillSwitch[]; metrics?: Metrics; now: string; }

const alerts = new Map<string, Alert>();
const feeds = new Map<string, Feed>();
let kills: KillSwitch[] = [];
let metrics: Metrics | null = null;
let selected: string | null = null;
let lastContact = 0;
let clockSkewMs = 0; // server now - local now, so we can age things on the server's clock
const rendered = new Set<string>();
const log: string[] = [];

const $ = <T extends HTMLElement>(sel: string) => document.querySelector(sel) as T;
const operatorInput = $<HTMLInputElement>('#operator');
try { operatorInput.value = localStorage.getItem('lens.operator') || ''; } catch { /* storage unavailable */ }
operatorInput.addEventListener('change', () => { try { localStorage.setItem('lens.operator', operatorInput.value.trim()); } catch { /* ignore */ } });

function operator(): string {
  const v = operatorInput.value.trim();
  if (!v) { operatorInput.focus(); operatorInput.style.borderColor = 'var(--bad)'; }
  return v;
}

// ---- connection ----

function connect() {
  const es = new EventSource('/v1/stream');
  es.addEventListener('snapshot', (e) => { lastContact = Date.now(); applySnapshot(JSON.parse((e as MessageEvent).data)); setConn('live', 'ok'); });
  es.addEventListener('event', (e) => { lastContact = Date.now(); applyEvent(JSON.parse((e as MessageEvent).data)); });
  es.addEventListener('heartbeat', (e) => { lastContact = Date.now(); const d = JSON.parse((e as MessageEvent).data); clockSkewMs = new Date(d.now).getTime() - Date.now(); });
  es.onerror = () => setConn('reconnecting', 'warn');
}

function setConn(text: string, cls: string) { const c = $('#conn'); c.textContent = text; c.className = 'chip ' + cls; }

setInterval(() => {
  const age = Date.now() - lastContact;
  const stale = lastContact === 0 || age > 10000;
  $('#stale-banner').classList.toggle('hidden', !stale);
  if (stale && lastContact) setConn(`stale ${Math.round(age / 1000)}s`, 'bad');
  renderSide(); // ages tick even without events
}, 1000);

// ---- state ----

function applySnapshot(fr: Frame) {
  alerts.clear(); feeds.clear();
  (fr.alerts || []).forEach(a => alerts.set(a.id, a));
  (fr.feeds || []).forEach(f => feeds.set(f.camera, f));
  kills = fr.kill_switches || [];
  metrics = fr.metrics || null;
  clockSkewMs = new Date(fr.now).getTime() - Date.now();
  renderAll();
  for (const a of alerts.values()) if (a.status === 'presented' && !rendered.has(a.id)) reportRendered(a);
}

function applyEvent(fr: Frame) {
  const ev = fr.event!;
  if (fr.alert && !fr.alert.probe) alerts.set(fr.alert.id, fr.alert);
  if (fr.feed) feeds.set(fr.feed.camera, fr.feed);
  if (fr.kill_switches) kills = fr.kill_switches;
  if (fr.metrics) metrics = fr.metrics;
  if (ev.type !== 'feed.heartbeat' && ev.type !== 'alert.rendered') {
    log.unshift(`${new Date(ev.ts).toLocaleTimeString()} ${ev.type} ${ev.actor || ''} ${short(ev.resource)}`);
    log.length = Math.min(log.length, 40);
  }
  renderAll();
  if (ev.type === 'alert.presented' && fr.alert) { reportRendered(fr.alert); beep(); }
  if (ev.type === 'alert.escalated') beep(3);
}

// The browser tells the core when the alert was actually on a human's screen.
// This closes the loop on time-to-deliver, which is the number that matters.
function reportRendered(a: Alert) {
  if (rendered.has(a.id)) return;
  rendered.add(a.id);
  requestAnimationFrame(() => {
    const at = new Date(Date.now() + clockSkewMs).toISOString();
    fetch(`/v1/alerts/${a.id}/rendered`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ rendered_at: at }) }).catch(() => undefined);
  });
}

async function act(id: string, action: string) {
  const op = operator(); if (!op) return;
  let note = '';
  if (action === 'reject') { note = prompt('Reason for rejection (optional):') || ''; }
  const r = await fetch(`/v1/alerts/${id}/${action}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ operator: op, note }) });
  if (!r.ok) { const e = await r.json().catch(() => ({})); alert(`Refused: ${e.error || r.status}`); }
}

async function toggleKill(agency: string, active: boolean) {
  const op = operator(); if (!op) return;
  const reason = active ? (prompt(`Kill switch for ${agency}: alerts will be committed but NOT shown to dispatchers. Reason:`) || '') : '';
  if (active && !reason) return;
  await fetch(`/v1/agencies/${encodeURIComponent(agency)}/killswitch`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ active, operator: op, reason }) });
}

// ---- rendering ----

const OPEN = new Set(['presented', 'acknowledged', 'validated', 'dispatched']);
const short = (lrn?: string) => (lrn || '').split('/').pop() || '';
const fmtMs = (v?: number) => v === undefined || Number.isNaN(v) ? '-' : v >= 1000 ? (v / 1000).toFixed(2) + ' s' : Math.round(v) + ' ms';
const ageS = (iso: string) => Math.max(0, (Date.now() + clockSkewMs - new Date(iso).getTime()) / 1000);

function renderAll() { renderQueue(); renderDetail(); renderSide(); renderHeader(); }

function renderHeader() {
  const open = [...alerts.values()].filter(a => OPEN.has(a.status)).length;
  const m = metrics;
  $('#hdr-stats').innerHTML = [
    `<span>open <b>${open}</b></span>`,
    m ? `<span>events <b>${m.last_seq}</b></span>` : '',
    m ? `<span>coverage <b>${(m.effective_coverage * 100).toFixed(1)}%</b></span>` : '',
    m && m.latency['capture_to_render'] ? `<span>capture→screen p95 <b>${fmtMs(m.latency['capture_to_render'].p95_ms)}</b></span>` : '',
  ].join('');
}

function renderQueue() {
  const open = [...alerts.values()].filter(a => OPEN.has(a.status))
    .sort((a, b) => Number(b.escalated) - Number(a.escalated) || b.timeline['committed_at'].localeCompare(a.timeline['committed_at']));
  $('#queue-count').textContent = String(open.length);
  const ul = $('#queue'); ul.innerHTML = '';
  if (!open.length) ul.innerHTML = '<li class="empty" style="border:none;cursor:default">No open alerts. Feeds below show whether LENS is watching.</li>';
  for (const a of open) {
    const li = document.createElement('li');
    li.className = (a.id === selected ? 'selected ' : '') + (a.escalated ? 'escalated' : '');
    li.innerHTML = `<div class="row"><span class="hazard">${a.hazard.replace(/_/g, ' ')}</span><span class="status ${a.status}">${a.status}</span></div>
      <div class="row meta"><span>${short(a.camera)} · ${a.agency}</span><span>${(a.confidence * 100).toFixed(0)}%</span></div>
      <div class="row meta"><span>${new Date(a.timeline['committed_at']).toLocaleTimeString()}</span><span>${a.escalated ? 'ESCALATED · ' : ''}${Math.round(ageS(a.timeline['presented_at'] || a.timeline['committed_at']))}s open</span></div>`;
    li.onclick = () => { selected = a.id; renderAll(); };
    ul.appendChild(li);
  }
  const hist = [...alerts.values()].filter(a => !OPEN.has(a.status)).sort((a, b) => b.timeline['committed_at'].localeCompare(a.timeline['committed_at'])).slice(0, 12);
  $('#history').innerHTML = hist.map(a => `<li><span class="status ${a.status}">${a.status}</span> ${a.hazard.replace(/_/g, ' ')} · ${short(a.camera)} · ${a.operator || 'system'} ${a.note ? '· ' + a.note : ''}</li>`).join('')
    + log.slice(0, 10).map(l => `<li class="small">${l}</li>`).join('');
}

function renderDetail() {
  const el = $('#detail');
  const a = selected ? alerts.get(selected) : undefined;
  if (!a) { el.innerHTML = '<div class="empty">Select an alert.</div>'; return; }
  const t = a.timeline, l = a.latency_ms;
  const hops: [string, string | undefined][] = [
    ['hazard onset (estimate)', t['onset_estimate']], ['frame captured', t['captured_at']], ['confirmed by model', t['confirmed_at']],
    ['emitted', t['emitted_at']], ['committed to log', t['committed_at']], ['presented', t['presented_at']], ['on operator screen', t['rendered_at']],
    ['acknowledged', t['acknowledged_at']], ['validated', t['validated_at']], ['rejected', t['rejected_at']], ['dispatched', t['dispatched_at']], ['closed', t['closed_at']], ['escalated', t['escalated_at']],
  ];
  const t0 = new Date(t['captured_at']).getTime();
  const rows = hops.filter(([, v]) => v && !v.startsWith('0001')).map(([k, v]) => `<tr><td>${k}</td><td>${new Date(v!).toLocaleTimeString()}.${String(new Date(v!).getMilliseconds()).padStart(3, '0')}</td><td class="num">${fmtMs(new Date(v!).getTime() - t0)}</td></tr>`).join('');
  const btn = (action: string, label: string, cls = '') => `<button class="${cls}" data-action="${action}">${label}</button>`;
  let actions = '';
  switch (a.status) {
    case 'presented': actions = btn('acknowledge', 'Acknowledge') + btn('validate', 'Validate', 'primary') + btn('reject', 'Reject', 'danger'); break;
    case 'acknowledged': actions = btn('validate', 'Validate: send officer', 'primary') + btn('reject', 'Reject', 'danger'); break;
    case 'validated': actions = btn('dispatch', 'Dispatched', 'primary') + btn('close', 'Close (no unit available)'); break;
    case 'dispatched': actions = btn('close', 'Close', 'primary'); break;
  }
  el.innerHTML = `
    ${a.evidence_url ? `<img src="${a.evidence_url}" alt="evidence still">` : '<div class="empty">no evidence still</div>'}
    <div class="title"><b>${a.hazard.replace(/_/g, ' ')}</b><span class="status ${a.status}">${a.status}${a.escalated ? ' · escalated' : ''}</span></div>
    <div class="actions">${actions}</div>
    <dl>
      <dt>camera</dt><dd>${a.camera}</dd>
      <dt>owner</dt><dd>${a.agency}</dd>
      <dt>confidence</dt><dd>${(a.confidence * 100).toFixed(1)}% · stationary ${a.stationary_seconds}s · bbox ${a.bbox.join(',')}</dd>
      <dt>model</dt><dd>${a.model_version}</dd>
      <dt>decision</dt><dd>${a.operator || '-'} ${a.note ? '· ' + a.note : ''}</dd>
      <dt>capture → commit</dt><dd>${fmtMs(l['capture_to_commit'])}</dd>
      <dt>capture → screen</dt><dd>${fmtMs(l['capture_to_render'])}</dd>
      <dt>onset → confirm</dt><dd>${fmtMs(l['onset_to_confirm'])} (model confirmation window)</dd>
      <dt>trace</dt><dd><a href="/v1/events?trace_id=${a.trace_id}" target="_blank">${a.trace_id}</a></dd>
      <dt>evidence sha256</dt><dd class="small">${a.evidence_sha256 || '-'}</dd>
    </dl>
    <div class="timeline"><table><thead><tr><th>hop</th><th>time</th><th>since capture</th></tr></thead><tbody>${rows}</tbody></table></div>`;
  el.querySelectorAll<HTMLButtonElement>('button[data-action]').forEach(b => b.onclick = () => act(a.id, b.dataset.action!));
}

function renderSide() {
  const fl = [...feeds.values()].sort((a, b) => a.camera.localeCompare(b.camera));
  $('#feeds tbody').innerHTML = fl.map(f => {
    const age = ageS(f.last_heartbeat);
    const secs = f.seconds || {}; const tot = Object.values(secs).reduce((s, v) => s + v, 0) || 1;
    const cov = ((secs['fresh'] || 0) / tot * 100).toFixed(0);
    return `<tr title="${f.source || ''}"><td><span class="dot ${f.status}"></span>${short(f.camera)}<div class="small">${f.agency} · watched ${cov}%</div></td>
      <td>${f.status}${f.scene_change ? ' · moved?' : ''}</td><td class="num">${age.toFixed(0)}s</td><td class="num">${f.fps.toFixed(1)}</td></tr>`;
  }).join('') || '<tr><td colspan="4" class="small">No feeds have reported. Nothing is being watched.</td></tr>';
  const m = metrics;
  $('#coverage').textContent = m ? `${(m.effective_coverage * 100).toFixed(1)}% watched` : '';
  const stages: [string, string][] = [['capture_to_commit', 'capture → commit'], ['commit_to_render', 'commit → screen'], ['capture_to_render', 'capture → screen'], ['present_to_ack', 'screen → acknowledge'], ['capture_to_dispatch', 'capture → dispatch'], ['onset_to_confirm', 'onset → confirm'], ['probe_capture_to_commit', 'probe: capture → commit']];
  $('#latency tbody').innerHTML = m ? stages.filter(([k]) => m.latency[k]).map(([k, n]) => `<tr><td>${n}</td><td class="num">${fmtMs(m.latency[k].p50_ms)}</td><td class="num">${fmtMs(m.latency[k].p95_ms)}</td><td class="num">${m.latency[k].n}</td></tr>`).join('') : '';
  const pr = $('#probe');
  if (m && m.probes.total) {
    const bad = m.probes.last_ok_age_s > 180;
    pr.className = 'probe' + (bad ? ' bad' : '');
    pr.innerHTML = `<b>Black-box probe</b>: ${m.probes.total} runs through the production path, last OK ${m.probes.last_ok_age_s.toFixed(0)}s ago${bad ? ' · <b>OVERDUE: page on-call</b>' : ''}.`;
  } else { pr.className = 'probe'; pr.innerHTML = '<b>Black-box probe</b>: no probe has completed yet.'; }
  const agencies = new Set<string>([...feeds.values()].map(f => f.agency).concat(kills.map(k => k.agency), [...alerts.values()].map(a => a.agency)));
  $('#killswitches').innerHTML = [...agencies].sort().map(ag => {
    const k = kills.find(x => x.agency === ag);
    const active = !!(k && k.active);
    return `<div class="ks ${active ? 'active' : ''}"><div><div>${ag}</div><div class="small">${active ? `suppressing since ${new Date(k!.since).toLocaleTimeString()} by ${k!.operator}: ${k!.reason}` : 'alerts flowing to dispatch'}</div></div>
      <button class="${active ? 'primary' : 'danger'}" data-agency="${ag}" data-active="${!active}">${active ? 'Release' : 'Kill switch'}</button></div>`;
  }).join('') || '<div class="small">No agencies yet.</div>';
  $('#killswitches').querySelectorAll<HTMLButtonElement>('button').forEach(b => b.onclick = () => toggleKill(b.dataset.agency!, b.dataset.active === 'true'));
}

// ---- audio cue ----
let audio: AudioContext | null = null;
function beep(n = 1) {
  try {
    audio = audio || new AudioContext();
    for (let i = 0; i < n; i++) {
      const o = audio.createOscillator(), g = audio.createGain();
      o.frequency.value = 880; g.gain.value = 0.05; o.connect(g); g.connect(audio.destination);
      o.start(audio.currentTime + i * 0.25); o.stop(audio.currentTime + i * 0.25 + 0.12);
    }
  } catch { /* autoplay policy or no audio */ }
}

connect();
