'use strict';

const els = {
  channels: document.getElementById('channels'),
  status: document.getElementById('status'),
  player: document.getElementById('player'),
  video: document.getElementById('video'),
  nowPlaying: document.getElementById('now-playing'),
  close: document.getElementById('close'),
  profile: document.getElementById('profile'),
  playerError: document.getElementById('player-error'),
  loading: document.getElementById('loading'),
};

let DEBUG = false; // enabled from /api/config when the server runs with -debug
function log(...a) { if (DEBUG) console.log('[hdhr]', ...a); }

let hls = null;          // active hls.js instance, if any
let currentChannel = null;
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
    if (currentChannel) play(currentChannel); // re-tune at new quality
  });
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

  if (stalledMs > 8000 && Date.now() - lastReload > 12000) {
    // Dry for >8s: soft recovery has failed. Reload the stream from scratch,
    // which re-fetches the playlist (and re-tunes the tuner if the server's
    // ffmpeg died). This is the only thing that recovers a fully-starved buffer.
    lastReload = Date.now();
    log('hard reload (stall recovery)');
    play(currentChannel);
  } else if (hls) {
    // Soft nudge: tell hls.js to (re)start loading; wake the element if paused.
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
    const cfg = await fetchJSON('/api/config');
    DEBUG = !!cfg.debug;
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
    const channels = await fetchJSON('/api/channels');
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
  // Only show a divider if both groups are non-empty.
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
  btn.addEventListener('click', () => play(ch));
  return btn;
}

function play(ch) {
  currentChannel = ch;
  wantPlaying = true;
  started = false;
  userPaused = false;
  const profile = els.profile.value;
  const src = `/stream/${encodeURIComponent(ch.number)}/index.m3u8?profile=${encodeURIComponent(profile)}`;

  els.player.classList.remove('hidden');
  els.nowPlaying.textContent = `${ch.number} · ${ch.name}`;
  hideError();
  showLoading();
  startWatchdog();
  teardownHls();

  const video = els.video;

  // Safari (incl. iPad/iPhone) plays HLS natively.
  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    video.src = src;
    video.play().catch(() => {});
    return;
  }

  if (window.Hls && Hls.isSupported()) {
    hls = new Hls({
      lowLatencyMode: false,
      liveSyncDurationCount: 4,       // sit ~4 segments behind live for cushion
      liveMaxLatencyDurationCount: 15,
      maxBufferLength: 30,
      backBufferLength: 30,
      fragLoadingMaxRetry: 6,
      levelLoadingMaxRetry: 6,
      manifestLoadingMaxRetry: 6,
      // The Extend's tuner lock + transcoder spin-up can take ~10s, during which
      // the server holds the first playlist request open. Tolerate that.
      manifestLoadingTimeOut: 25000,
      levelLoadingTimeOut: 25000,
      // Be forgiving of small timestamp gaps in the remuxed stream so playback
      // skips them instead of stalling.
      maxBufferHole: 0.5,
      nudgeMaxRetry: 10,
    });
    hls.loadSource(src);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, () => video.play().catch(() => {}));
    hls.on(Hls.Events.ERROR, (_evt, data) => {
      log('hls error', data.type, data.details, 'fatal=' + data.fatal);
      if (data.details === Hls.ErrorDetails.BUFFER_STALLED_ERROR) {
        hls.startLoad(); // non-fatal; the watchdog escalates to a reload if needed
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
          showError(`Playback error (${data.type}). The tuner may be busy or starting up — try again.`);
      }
    });
    return;
  }

  showError('This browser cannot play HLS.');
}

function stopPlayback() {
  wantPlaying = false;
  stopWatchdog();
  teardownHls();
  els.video.removeAttribute('src');
  els.video.load();
  els.player.classList.add('hidden');
  currentChannel = null;
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
