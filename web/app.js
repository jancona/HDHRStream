'use strict';

const els = {
  views: document.getElementById('views'),
  viewLive: document.getElementById('view-live'),
  viewRecordings: document.getElementById('view-recordings'),
  channels: document.getElementById('channels'),
  recordings: document.getElementById('recordings'),
  series: document.getElementById('series'),
  episodes: document.getElementById('episodes'),
  episodesTitle: document.getElementById('episodes-title'),
  episodeList: document.getElementById('episode-list'),
  episodesBack: document.getElementById('episodes-back'),
  status: document.getElementById('status'),
  player: document.getElementById('player'),
  video: document.getElementById('video'),
  nowPlaying: document.getElementById('now-playing'),
  close: document.getElementById('close'),
  profile: document.getElementById('profile'),
  playerError: document.getElementById('player-error'),
  loading: document.getElementById('loading'),
};

let DEBUG = false;       // enabled from /api/config when the server runs with -debug
let RECORDINGS = false;  // enabled when a DVR is configured
function log(...a) { if (DEBUG) console.log('[hdhr]', ...a); }

let hls = null;          // active hls.js instance, if any
let current = null;      // {src, title, busyMsg, live} of what we're playing
let seriesLoaded = false;
let wantPlaying = false; // we intend the stream to be playing
let started = false;     // playback has begun at least once (past the tuning phase)
let userPaused = false;  // the user deliberately paused; don't auto-resume
let lastGesture = 0;     // timestamp of the last user interaction with the player
let watchdog = null;     // interval that recovers from stall-induced pauses

async function init() {
  await loadConfig();
  await loadChannels();

  els.close.addEventListener('click', stopPlayback);
  els.profile.addEventListener('change', () => {
    if (current) playStream(current.src, current.title, current.busyMsg, current.live); // re-tune at new quality
  });
  if (RECORDINGS) {
    els.views.classList.remove('hidden');
    els.viewLive.addEventListener('click', () => showView('live'));
    els.viewRecordings.addEventListener('click', () => showView('recordings'));
    els.episodesBack.addEventListener('click', () => els.episodes.classList.add('hidden'));
  }

  // A live stream pauses (rather than silently re-buffering) when its buffer
  // drains on a network hiccup — notably on iOS Safari. We want to auto-resume
  // those, but respect a deliberate pause. We tell them apart by whether the
  // user just interacted with the player.
  for (const evt of ['pointerdown', 'keydown', 'touchstart']) {
    els.video.addEventListener(evt, () => { lastGesture = Date.now(); });
  }
  els.video.addEventListener('pause', onVideoPause);
  els.video.addEventListener('play', () => { userPaused = false; });
  // Hide the "Tuning…" overlay once frames are actually flowing.
  els.video.addEventListener('playing', () => { started = true; hideLoading(); });

  // Diagnostic trace: shows what the player is doing during a stall.
  for (const e of ['waiting', 'stalled', 'playing', 'pause', 'play', 'ended', 'emptied', 'seeking', 'seeked']) {
    els.video.addEventListener(e, () => {
      const v = els.video;
      log('video', e, 't=' + v.currentTime.toFixed(1), 'ready=' + v.readyState, 'paused=' + v.paused);
    });
  }
}

function showView(view) {
  const live = view === 'live';
  els.viewLive.classList.toggle('active', live);
  els.viewRecordings.classList.toggle('active', !live);
  els.channels.classList.toggle('hidden', !live);
  els.recordings.classList.toggle('hidden', live);
  if (!live && !seriesLoaded) loadSeries();
}

function showLoading() { els.loading.classList.remove('hidden'); }
function hideLoading() { els.loading.classList.add('hidden'); }

function onVideoPause() {
  if (!wantPlaying || els.video.ended) return;
  // A pause within ~600ms of a tap/click/keypress is the user pausing on purpose.
  // Otherwise it's a stall-induced pause; the watchdog handles recovery.
  if (Date.now() - lastGesture < 600) userPaused = true;
}

// startWatchdog backs up the event handlers by watching whether playback time is
// actually advancing. Chromium/hls.js often *stalls silently* (no pause event)
// on a buffer underrun, so paused-state alone isn't enough. Recovery escalates:
// a soft nudge first, then a full reload once the buffer has been dry a while —
// because a starved live buffer can't be "nudged" back, only re-fetched.
let lastTime = -1;
let lastAdvance = 0;
let lastReload = 0;
function startWatchdog() {
  stopWatchdog();
  lastTime = -1;
  lastAdvance = Date.now();
  lastReload = 0;
  watchdog = setInterval(tickWatchdog, 2000);
}

function tickWatchdog() {
  const v = els.video;
  if (!wantPlaying || !started || userPaused || document.hidden || v.seeking) return;
  if (v.currentTime !== lastTime) { // progressing normally
    lastTime = v.currentTime;
    lastAdvance = Date.now();
    return;
  }
  // currentTime hasn't moved since last check — we're stalled.
  const stalledMs = Date.now() - lastAdvance;
  log('no progress', (stalledMs / 1000).toFixed(1) + 's', 't=' + v.currentTime.toFixed(1),
    'paused=' + v.paused, 'ready=' + v.readyState, 'buffered=' + bufferedAhead(v).toFixed(1) + 's');

  // Hard reload only makes sense for live: a starved live buffer can't be nudged,
  // only re-fetched. For a recording, reloading would restart the transcode from
  // the beginning, so we only ever soft-nudge and let it wait for segments.
  if (current && current.live && stalledMs > 8000 && Date.now() - lastReload > 12000) {
    lastReload = Date.now();
    log('hard reload (stall recovery)');
    playStream(current.src, current.title, current.busyMsg, current.live);
  } else if (hls) {
    hls.startLoad();
    if (v.paused) v.play().catch(() => {});
  }
}

function bufferedAhead(v) {
  for (let i = 0; i < v.buffered.length; i++) {
    if (v.currentTime <= v.buffered.end(i) + 0.25) return v.buffered.end(i) - v.currentTime;
  }
  return 0;
}

function stopWatchdog() {
  if (watchdog) {
    clearInterval(watchdog);
    watchdog = null;
  }
}

async function loadConfig() {
  try {
    const cfg = await fetchJSON('api/config');
    DEBUG = !!cfg.debug;
    RECORDINGS = !!cfg.recordings;
    els.profile.innerHTML = '';
    for (const p of cfg.profiles) {
      const opt = document.createElement('option');
      opt.value = p;
      opt.textContent = p;
      if (p === cfg.defaultProfile) opt.selected = true;
      els.profile.appendChild(opt);
    }
  } catch (e) {
    console.warn('config load failed', e);
  }
}

async function loadChannels() {
  els.status.textContent = 'Loading channels…';
  try {
    const channels = await fetchJSON('api/channels');
    renderChannels(channels);
    els.status.textContent = channels.length ? '' : 'No channels found.';
  } catch (e) {
    els.status.textContent = 'Could not load channels: ' + e.message;
  }
}

function renderChannels(channels) {
  els.channels.innerHTML = '';
  const favorites = channels.filter((ch) => ch.favorite);
  const others = channels.filter((ch) => !ch.favorite);

  for (const ch of favorites) els.channels.appendChild(channelButton(ch));
  if (favorites.length && others.length) {
    const sep = document.createElement('div');
    sep.className = 'separator';
    sep.textContent = 'All Channels';
    els.channels.appendChild(sep);
  }
  for (const ch of others) els.channels.appendChild(channelButton(ch));
}

function channelButton(ch) {
  const btn = document.createElement('button');
  btn.className = 'channel';
  btn.type = 'button';
  btn.innerHTML =
    `<span class="num">${escapeHTML(ch.number)}</span>` +
    (ch.favorite ? '<span class="star" aria-label="favorite">★</span>' : '') +
    (ch.hd ? '<span class="hd">HD</span>' : '') +
    `<span class="name">${escapeHTML(ch.name)}</span>`;
  btn.addEventListener('click', () => playChannel(ch));
  return btn;
}

// --- Recordings ------------------------------------------------------------

async function loadSeries() {
  seriesLoaded = true;
  els.series.innerHTML = '<p class="status">Loading recordings…</p>';
  try {
    const series = await fetchJSON('api/recordings');
    els.series.innerHTML = '';
    if (!series.length) {
      els.series.innerHTML = '<p class="status">No recordings.</p>';
      return;
    }
    for (const s of series) els.series.appendChild(seriesCard(s));
  } catch (e) {
    seriesLoaded = false;
    els.series.innerHTML = `<p class="status">Could not load recordings: ${escapeHTML(e.message)}</p>`;
  }
}

function seriesCard(s) {
  const btn = document.createElement('button');
  btn.className = 'series-card';
  btn.type = 'button';
  const img = s.image ? `<img loading="lazy" src="${escapeHTML(s.image)}" alt="">` : '<div class="noart"></div>';
  btn.innerHTML = `${img}<span class="series-title">${escapeHTML(s.title)}</span>`;
  btn.addEventListener('click', () => loadEpisodes(s));
  return btn;
}

async function loadEpisodes(s) {
  els.episodesTitle.textContent = s.title;
  els.episodeList.innerHTML = '<p class="status">Loading…</p>';
  els.episodes.classList.remove('hidden');
  try {
    const eps = await fetchJSON('api/recordings/' + encodeURIComponent(s.id));
    els.episodeList.innerHTML = '';
    for (const ep of eps) els.episodeList.appendChild(episodeRow(ep));
  } catch (e) {
    els.episodeList.innerHTML = `<p class="status">Could not load episodes: ${escapeHTML(e.message)}</p>`;
  }
}

function episodeRow(ep) {
  const btn = document.createElement('button');
  btn.className = 'episode';
  btn.type = 'button';
  // Series title is already the header; lead each row with the episode title,
  // else the episode number, else the recording date.
  const head = ep.subtitle || ep.episode || formatDate(ep.recordedAt) || ep.title;
  const meta = [ep.subtitle ? ep.episode : null, ep.channel, formatDate(ep.recordedAt), formatDuration(ep.duration)]
    .filter(Boolean).join(' · ');
  btn.innerHTML =
    `<span class="ep-head">${escapeHTML(head)}</span>` +
    (meta ? `<span class="ep-meta">${escapeHTML(meta)}</span>` : '') +
    (ep.synopsis ? `<span class="ep-synopsis">${escapeHTML(ep.synopsis)}</span>` : '');
  btn.addEventListener('click', () => playRecording(ep));
  return btn;
}

function formatDate(unix) {
  if (!unix) return '';
  return new Date(unix * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
}

function formatDuration(secs) {
  if (!secs) return '';
  return Math.round(secs / 60) + ' min';
}

// --- Playback --------------------------------------------------------------

function playChannel(ch) {
  const profile = els.profile.value;
  const rel = `stream/${encodeURIComponent(ch.number)}/index.m3u8?profile=${encodeURIComponent(profile)}`;
  playStream(new URL(rel, document.baseURI).href, `${ch.number} · ${ch.name}`,
    'All tuners are in use. Another stream — or an HDHomeRun recording — may be using the tuner. Stop one and try again.',
    true);
}

function playRecording(ep) {
  const profile = els.profile.value;
  const rel = `rec/${encodeURIComponent(ep.id)}/index.m3u8?profile=${encodeURIComponent(profile)}`;
  const title = ep.episode ? `${ep.title} · ${ep.episode}` : ep.title;
  playStream(new URL(rel, document.baseURI).href, title,
    'The server is busy transcoding other recordings. Try again shortly.',
    false);
}

async function playStream(src, title, busyMsg, live) {
  current = { src, title, busyMsg, live };
  wantPlaying = true;
  started = false;
  userPaused = false;

  els.player.classList.remove('hidden');
  els.nowPlaying.textContent = title;
  hideError();
  showLoading();
  teardownHls();

  // Pre-flight: this request starts the stream/transcode, and lets us turn a
  // busy/availability failure into a clear message instead of a hung spinner.
  let resp;
  try {
    resp = await fetch(src, { cache: 'no-store' });
  } catch {
    if (current && current.src === src) failPlayback('Could not reach the server.');
    return;
  }
  if (!current || current.src !== src || !wantPlaying) return; // superseded by a newer selection
  if (resp.status === 503) {
    failPlayback(busyMsg);
    return;
  }
  if (!resp.ok) {
    failPlayback(live
      ? 'Could not start this channel. It may be unavailable or have no signal.'
      : 'Could not start this recording — the server may be transcoding too slowly for this quality. Try a lower quality.');
    return;
  }

  startWatchdog();
  const video = els.video;

  // Safari (incl. iPad/iPhone) plays HLS natively.
  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = src;
    video.play().catch(() => {});
    return;
  }

  if (window.Hls && Hls.isSupported()) {
    const cfg = {
      lowLatencyMode: false,
      maxBufferLength: 30,
      fragLoadingMaxRetry: 6,
      levelLoadingMaxRetry: 6,
      manifestLoadingMaxRetry: 6,
      // The Extend's tuner lock + transcoder spin-up can take ~10s, during which
      // the server holds the first playlist request open. Tolerate that.
      manifestLoadingTimeOut: 25000,
      levelLoadingTimeOut: 25000,
      // Be forgiving of small timestamp gaps in the stream so playback skips them
      // instead of stalling.
      maxBufferHole: 0.5,
      nudgeMaxRetry: 10,
    };
    if (live) {
      cfg.liveSyncDurationCount = 4;        // sit ~4 segments behind live for cushion
      cfg.liveMaxLatencyDurationCount = 15;
      cfg.backBufferLength = 30;
    } else {
      // A recording is a growing VOD that transcodes ahead of the playhead. Start
      // at the beginning and DON'T chase the (fast-moving) edge, so pause builds
      // slack and rewind/skip work within what's been transcoded.
      cfg.startPosition = 0;
      cfg.backBufferLength = 90;
    }
    hls = new Hls(cfg);
    if (DEBUG) window._hls = hls; // expose for console inspection
    hls.loadSource(src);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, () => video.play().catch(() => {}));
    hls.on(Hls.Events.ERROR, (_evt, data) => {
      log('hls error', data.type, data.details, 'fatal=' + data.fatal);
      if (data.details === Hls.ErrorDetails.BUFFER_STALLED_ERROR) {
        hls.startLoad(); // non-fatal; the watchdog escalates if needed
        return;
      }
      if (!data.fatal) return;
      switch (data.type) {
        case Hls.ErrorTypes.NETWORK_ERROR:
          hls.startLoad(); // recover from a transient VPN/network blip
          els.video.play().catch(() => {});
          break;
        case Hls.ErrorTypes.MEDIA_ERROR:
          hls.recoverMediaError();
          els.video.play().catch(() => {});
          break;
        default:
          showError(`Playback error (${data.type}). Try again.`);
      }
    });
    return;
  }

  failPlayback('This browser cannot play HLS.');
}

function stopPlayback() {
  wantPlaying = false;
  stopWatchdog();
  teardownHls();
  els.video.removeAttribute('src');
  els.video.load();
  els.player.classList.add('hidden');
  current = null;
  hideError();
  hideLoading();
}

function teardownHls() {
  if (hls) {
    hls.destroy();
    hls = null;
  }
}

function showError(msg) {
  hideLoading();
  els.playerError.textContent = msg;
  els.playerError.classList.remove('hidden');
}

// failPlayback gives up on the current attempt: stops the recovery watchdog and
// shows a message, so a busy/availability failure reads clearly instead of
// spinning forever.
function failPlayback(msg) {
  wantPlaying = false;
  stopWatchdog();
  showError(msg);
}

function hideError() {
  els.playerError.classList.add('hidden');
  els.playerError.textContent = '';
}

async function fetchJSON(url) {
  const resp = await fetch(url);
  if (!resp.ok) throw new Error(resp.status + ' ' + resp.statusText);
  return resp.json();
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])
  );
}

init();
