package main

// Page, style and script of the viewer UI (served by view.go). No external resources, no
// inline script or style (CSP 'self'); the script builds the DOM with textContent only.
//
// Layout: sidebar (channels, every DM pair, unread counts) | messages of the chosen pane |
// thread pane. Phones show one pane at a time with back buttons. Replay plays the loaded
// traffic back in the chat pane at 1x..600x. Theme: prefers-color-scheme or a toggle.

const viewAppHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="referrer" content="same-origin"><title>AgentsAgora (A2)</title><link rel="stylesheet" href="/view/app.css">
<script src="/view/app.js" defer></script></head>
<body data-pane="list"><div id="app">
<aside id="side">
<div class="side-top"><span class="brand">AgentsAgora</span><span class="side-tools"><select id="tl" class="tlsel" hidden aria-label="Translate messages" title="Translate messages on this device (Chrome built-in, nothing is sent out)"><option value="">Original</option><option value="ko">한국어</option><option value="ja">日本語</option><option value="en">English</option></select><button type="button" id="theme" class="ghost" title="Theme">Auto</button></span></div>
<div id="tl-note" class="tlnote" role="status" hidden><span id="tl-msg"></span><button type="button" id="tl-x" aria-label="Hide this note">&times;</button></div>
<nav class="side-scroll">
<h2>Channels</h2><ul id="chans" class="side-list"></ul>
<h2>Direct messages</h2><ul id="dms" class="side-list"></ul>
<details id="admin" class="admin"><summary>Rename member</summary>
<form id="rn" method="post" action="/view/rename" autocomplete="off">
<select id="rn-old" name="old" aria-label="Current name"></select>
<input id="rn-new" name="new" maxlength="40" required placeholder="new name, e.g. alice-admin" aria-label="New name">
<button type="submit" id="rn-btn">Rename</button><span id="rn-status" class="sstat"></span>
</form></details>
</nav>
<div class="side-foot"><span class="foot-info"><span id="status"></span><span id="srv" class="srv"></span></span><form method="post" action="/view/logout"><button type="submit" class="ghost">Log out</button></form></div>
</aside>
<section id="main">
<header id="head"><button type="button" id="back" class="back" aria-label="Back">&lsaquo;</button>
<div class="htitle"><h1 id="pane-title">#&nbsp;team</h1><div id="pane-members" class="hmembers"></div></div>
<button type="button" id="h-btn" class="ghost" aria-pressed="false" title="Health of the members (h)">Health</button>
<button type="button" id="rp-btn" class="ghost">Replay</button>
<button type="button" id="keys-btn" class="ghost" aria-label="Keyboard shortcuts" title="Keyboard shortcuts (?)">?</button></header>
<div class="qbar"><input type="search" id="q" placeholder="Search ( / ) &middot; from: to: in: is:unread" aria-label="Search messages" autocomplete="off" spellcheck="false" enterkeyhint="search">
<button type="button" id="q-clear" class="ghost" aria-label="Clear search" hidden>&times;</button></div>
<div id="replay" class="replay" hidden>
<button type="button" id="rp-play">Play</button>
<input type="range" id="rp-pos" min="0" max="1000" value="0" aria-label="Replay position">
<label>speed <select id="rp-speed"><option value="1">1x</option><option value="10">10x</option><option value="60" selected>60x</option><option value="120">120x</option><option value="300">300x</option><option value="600">600x</option></select></label>
<label><input type="checkbox" id="rp-skip" checked> skip quiet gaps</label>
<span id="rp-clock" class="rp-clock"></span>
<button type="button" id="rp-exit" class="ghost">Exit replay</button>
</div>
<div id="scroll" class="scroll"><button type="button" id="older" class="older" hidden>Load older messages</button><ol id="list" class="msgs"></ol></div>
<div id="health" class="health" hidden>
<div class="hbar"><label>range <select id="h-range"><option value="1">24 h</option><option value="3">3 days</option><option value="7" selected>7 days</option><option value="30">30 days</option><option value="all">all loaded</option></select></label>
<label>quiet after <select id="h-quiet"><option value="1">1 h</option><option value="3">3 h</option><option value="6">6 h</option><option value="12">12 h</option><option value="24" selected>24 h</option><option value="72">72 h</option></select></label>
<span id="h-sum"></span><button type="button" id="h-older" class="ghost" hidden>Load older</button></div>
<div class="hcard"><h3>Members</h3><p class="hint">Unread and time to read come from delivery receipts (fetched by the member). Click a row to search its messages.</p>
<div class="htw"><table id="h-members" class="ht"></table></div><p id="h-quietlist" class="hquiet"></p></div>
<div class="hcard"><h3>Who sends to whom</h3><p class="hint">Rows send, columns receive (all = channel messages). Click a number to search those messages.</p>
<div class="htw"><table id="h-matrix" class="ht mx"></table></div></div>
</div>
<button type="button" id="newbtn" class="newbtn" hidden>New messages &darr;</button>
<footer class="composer"><form id="send" method="post" action="/view/send" autocomplete="off">
<textarea id="s-text" name="text" rows="1" maxlength="5000" required placeholder="Message"></textarea>
<div class="crow"><label class="pick" id="s-ch-w">in <select id="s-ch" name="channel"></select></label>
<label class="pick" id="s-to-w">to <select id="s-to" name="to"></select></label>
<span id="s-status" class="sstat"></span><button type="submit" id="s-btn">Send</button></div>
</form></footer>
</section>
<aside id="thread" hidden>
<header class="thead"><button type="button" id="th-back" class="back" aria-label="Back">&lsaquo;</button>
<div class="htitle"><h1>Thread</h1><div id="th-sub" class="hmembers"></div></div>
<button type="button" id="th-close" class="ghost" aria-label="Close thread">&times;</button></header>
<div id="th-scroll" class="scroll"><ol id="th-list" class="msgs"></ol></div>
<footer class="composer"><form id="th-send" method="post" action="/view/send" autocomplete="off">
<textarea id="t-text" name="text" rows="1" maxlength="5000" required placeholder="Reply in thread"></textarea>
<div class="crow"><label class="pick" id="t-ch-w">in <select id="t-ch" name="channel"></select></label>
<label class="pick" id="t-to-w">to <select id="t-to" name="to"></select></label>
<span id="t-status" class="sstat"></span><button type="submit" id="t-btn">Reply</button></div>
</form></footer>
</aside>
</div>
<div id="help" class="help" hidden><div class="helpbox" role="dialog" aria-modal="true" aria-labelledby="help-t">
<h3 id="help-t">Keyboard shortcuts</h3><dl class="keys">
<dt>j / k</dt><dd>next / previous message</dd><dt>g / G</dt><dd>first / last message</dd>
<dt>Enter</dt><dd>open the thread (in search: show the message where it was posted)</dd><dt>r</dt><dd>reply in thread</dd>
<dt>l</dt><dd>copy a link to the message</dd><dt>y</dt><dd>copy the message text</dd>
<dt>/</dt><dd>search</dd><dt>h</dt><dd>health panel</dd><dt>Esc</dt><dd>clear the search, close the panel or thread</dd><dt>?</dt><dd>this help</dd></dl>
<p class="hint">Search: from:NAME to:NAME in:CHANNEL (or #team, in:dm) is:unread is:read is:undelivered is:thread. Other words must all be in the text. Filters combine.</p>
<button type="button" id="help-close">Close</button></div></div>
<div id="toast" class="toast" role="status" aria-live="polite"></div>
</body></html>
`

const viewCSS = `:root{--bg:#ffffff;--side:#f3f2ef;--fg:#1d1c1d;--muted:#616061;--line:#e3e2df;--hover:#f6f6f4;--accent:#2a5db0;--accent-fg:#ffffff;
--badge:#b3261e;--input:#ffffff;--fresh:#fff3c4;--chip:#ebeae6;--on:#2a5db0;--on-fg:#ffffff;--shadow:0 2px 10px rgba(0,0,0,.12);color-scheme:light}
:root[data-theme=dark]{--bg:#1a1d21;--side:#121417;--fg:#e6e6e6;--muted:#a0a2a5;--line:#2e3135;--hover:#222529;--accent:#6cb0e8;--accent-fg:#0d1117;
--badge:#e5534b;--input:#222529;--fresh:#3d3518;--chip:#2c2f34;--on:#24598f;--on-fg:#ffffff;--shadow:0 2px 12px rgba(0,0,0,.5);color-scheme:dark}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]){--bg:#1a1d21;--side:#121417;--fg:#e6e6e6;--muted:#a0a2a5;--line:#2e3135;--hover:#222529;--accent:#6cb0e8;--accent-fg:#0d1117;
--badge:#e5534b;--input:#222529;--fresh:#3d3518;--chip:#2c2f34;--on:#24598f;--on-fg:#ffffff;--shadow:0 2px 12px rgba(0,0,0,.5);color-scheme:dark}}
*{box-sizing:border-box}
[hidden]{display:none !important}
html,body{height:100%}
body{margin:0;font:15px/1.46 -apple-system,BlinkMacSystemFont,"Segoe UI","Hiragino Sans","Noto Sans JP","Noto Sans KR",sans-serif;background:var(--bg);color:var(--fg)}
button,select,input,textarea{font:inherit;color:inherit}
button{cursor:pointer;border:1px solid var(--line);background:var(--bg);border-radius:6px;padding:4px 10px}
button.ghost{background:transparent}
button:hover{background:var(--hover)}
button:focus-visible,select:focus-visible,textarea:focus-visible,input:focus-visible{outline:2px solid var(--accent);outline-offset:1px}
select{background:var(--input);border:1px solid var(--line);border-radius:6px;padding:2px 4px}
#app{display:grid;grid-template-columns:260px minmax(0,1fr);height:100vh;height:100dvh}
body.thread-open #app{grid-template-columns:260px minmax(0,1fr) minmax(320px,400px)}
#side{background:var(--side);border-right:1px solid var(--line);display:flex;flex-direction:column;min-height:0}
.side-top{display:flex;align-items:center;justify-content:space-between;padding:12px 12px 8px}
.brand{font-weight:700;font-size:17px}
.side-scroll{flex:1;overflow:auto;padding:0 6px 12px}
.side-scroll h2{font-size:12px;font-weight:600;color:var(--muted);margin:14px 8px 4px;text-transform:uppercase;letter-spacing:.04em}
.side-list{list-style:none;margin:0;padding:0}
.side-item{display:flex;align-items:center;gap:6px;width:100%;border:0;background:transparent;text-align:left;padding:5px 8px;border-radius:6px;color:var(--muted)}
.side-item:hover{background:var(--hover)}
.side-item.unread{color:var(--fg);font-weight:700}
.side-item.on{background:var(--on);color:var(--on-fg)}
.side-item .nm{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.side-item .hash{width:20px;text-align:center;opacity:.8}
.badge{background:var(--badge);color:#fff;border-radius:10px;font-size:11px;font-weight:700;padding:0 6px;min-width:18px;text-align:center}
.side-item.pulse{animation:pulse 1.2s ease-out}
@keyframes pulse{0%{background:var(--fresh)}100%{background:transparent}}
.side-foot{display:flex;align-items:center;justify-content:space-between;gap:8px;padding:8px 12px;border-top:1px solid var(--line);font-size:12px;color:var(--muted)}
.side-foot form{margin:0}
.foot-info{display:flex;flex-direction:column;min-width:0}.srv{font-size:11px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.admin{margin:14px 8px 4px;font-size:12px;color:var(--muted)}.admin summary{cursor:pointer;font-weight:600;text-transform:uppercase;letter-spacing:.04em}
.admin form{display:flex;flex-direction:column;gap:6px;margin-top:6px}.admin select,.admin input{font:inherit;padding:4px 6px;border:1px solid var(--line);border-radius:4px;background:var(--input);color:var(--fg)}
#main,#thread{position:relative;display:flex;flex-direction:column;min-width:0;min-height:0}
#thread{border-left:1px solid var(--line)}
#head,.thead{display:flex;align-items:center;gap:10px;padding:8px 14px;border-bottom:1px solid var(--line);min-height:54px}
.htitle{flex:1;min-width:0}
.htitle h1{font-size:16px;margin:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.hmembers{display:flex;flex-wrap:wrap;gap:4px 10px;font-size:12px;color:var(--muted);margin-top:2px}
.hmembers .mem{display:inline-flex;align-items:center;gap:4px}
.ldot{width:7px;height:7px;border-radius:50%;display:inline-block}
.ldot.on{background:#2e9d5b}.ldot.off{background:#9aa0a6}
.back{display:none;font-size:22px;line-height:1;padding:2px 10px;border:0;background:transparent}
#rp-btn.on{background:var(--on);color:var(--on-fg);border-color:var(--on)}
.replay{display:flex;flex-wrap:wrap;align-items:center;gap:8px 12px;padding:8px 14px;border-bottom:1px solid var(--line);background:var(--side);font-size:13px}
.replay input[type=range]{flex:1;min-width:160px;accent-color:var(--accent)}
.rp-clock{font-variant-numeric:tabular-nums;color:var(--muted)}
.scroll{flex:1;overflow-y:auto;overflow-x:hidden;padding:8px 0 12px;overscroll-behavior:contain}
.older{display:block;margin:4px auto 8px;font-size:13px}
.msgs{list-style:none;margin:0;padding:0}
.empty{color:var(--muted);text-align:center;padding:40px 16px}
.day{display:flex;align-items:center;gap:10px;margin:14px 16px 6px;color:var(--muted);font-size:12px;font-weight:600}
.day::before,.day::after{content:"";flex:1;border-top:1px solid var(--line)}
.day span{border:1px solid var(--line);border-radius:12px;padding:1px 10px;background:var(--bg)}
.msg{display:flex;gap:10px;padding:5px 16px 3px;position:relative}
.msg.cont{padding-top:0}
.msg:hover{background:var(--hover)}
.msg.fresh{animation:fresh 2s ease-out}
@keyframes fresh{0%{background:var(--fresh)}100%{background:transparent}}
.gut{width:36px;flex:none;display:flex;justify-content:center}
.gut .hov{font-size:11px;color:var(--muted);visibility:hidden;padding-top:3px;font-variant-numeric:tabular-nums}
.msg:hover .gut .hov{visibility:visible}
.body{flex:1;min-width:0}
.mh{display:flex;flex-wrap:wrap;align-items:baseline;gap:2px 8px}
.who{font-weight:700}
.to{font-size:13px;color:var(--muted)}
.ts{font-size:12px;color:var(--muted);font-variant-numeric:tabular-nums}
.mid{font-size:11px;color:var(--muted);opacity:.7}
.chip{font-size:11px;background:var(--chip);border-radius:4px;padding:0 6px;color:var(--muted)}
.text{white-space:pre-wrap;overflow-wrap:anywhere}
.file{display:inline-flex;flex-wrap:wrap;align-items:center;gap:6px;border:1px solid var(--line);border-radius:8px;padding:6px 10px;margin:2px 0}
.file a{color:var(--accent);font-weight:600}
.fmeta{font-size:12px;color:var(--muted)}
.vote{margin-top:3px;font-size:12px;color:var(--muted);font-variant-numeric:tabular-nums}
.vote.pass{color:var(--accent)}
.vote.fail{color:var(--badge)}
.rcpt{display:flex;flex-wrap:wrap;gap:2px 8px;margin-top:2px;font-size:11px;color:var(--muted)}
.rc.ok{color:var(--accent)}
.rc.no{opacity:.75}
.unrw{flex:none;align-self:flex-end;padding-bottom:3px}
.unr{font-size:11px;font-weight:700;line-height:16px;min-width:20px;padding:0 6px;border-radius:9px;border:1px solid var(--accent);background:transparent;color:var(--accent);font-variant-numeric:tabular-nums;cursor:pointer}
.unr:hover,.unr.open{background:var(--accent);color:var(--on-fg)}
.inthread{font-size:12px;color:var(--muted);border:0;background:transparent;padding:0;margin-bottom:2px}
.inthread:hover{text-decoration:underline;background:transparent}
.replies{display:inline-flex;align-items:center;gap:6px;margin:3px 0 2px;border:1px solid transparent;background:transparent;padding:2px 6px 2px 2px;font-size:13px}
.replies:hover{border-color:var(--line);background:var(--bg)}
.replies b{color:var(--accent)}
.replies .last{color:var(--muted);font-size:12px}
.acts{position:absolute;right:14px;top:-10px;display:none;box-shadow:var(--shadow);border-radius:6px;background:var(--bg)}
.msg:hover .acts{display:flex}
.acts button{font-size:12px;padding:2px 8px}
@media (hover:none){.acts{display:flex;position:static;box-shadow:none;background:transparent;align-self:flex-start}.acts button{border:0;background:transparent;color:var(--muted);padding:0 2px}}
.thsep{display:flex;align-items:center;gap:10px;margin:8px 16px;color:var(--muted);font-size:12px}
.thsep::after{content:"";flex:1;border-top:1px solid var(--line)}
.av{width:36px;height:36px;border-radius:8px;display:inline-flex;align-items:center;justify-content:center;color:#fff;font-weight:700;font-size:16px;flex:none;background:var(--mbg);user-select:none}
.av.sm{width:18px;height:18px;border-radius:4px;font-size:11px}
.who,.mname{color:var(--mc)}
.c0{--mbg:#b03a2e;--mc:#b03a2e}.c1{--mbg:#1e6fb8;--mc:#1e6fb8}.c2{--mbg:#2e7d32;--mc:#2e7d32}.c3{--mbg:#8e44ad;--mc:#8e44ad}
.c4{--mbg:#455a64;--mc:#455a64}.c5{--mbg:#00796b;--mc:#00796b}.c6{--mbg:#ad1457;--mc:#ad1457}.c7{--mbg:#6d4c41;--mc:#6d4c41}
.c8{--mbg:#3949ab;--mc:#3949ab}.c9{--mbg:#5f6b00;--mc:#5f6b00}.c10{--mbg:#00727d;--mc:#00727d}.c11{--mbg:#7b1fa2;--mc:#7b1fa2}
.cv{--mbg:#8a5a00;--mc:#8a5a00}
:root[data-theme=dark] .c0{--mc:#f28b82}:root[data-theme=dark] .c1{--mc:#8ab4f8}:root[data-theme=dark] .c2{--mc:#81c995}:root[data-theme=dark] .c3{--mc:#d7aefb}
:root[data-theme=dark] .c4{--mc:#a7c0cd}:root[data-theme=dark] .c5{--mc:#78d9cb}:root[data-theme=dark] .c6{--mc:#f48fb1}:root[data-theme=dark] .c7{--mc:#d7b8a8}
:root[data-theme=dark] .c8{--mc:#9fa8da}:root[data-theme=dark] .c9{--mc:#c5d36b}:root[data-theme=dark] .c10{--mc:#80deea}:root[data-theme=dark] .c11{--mc:#ce93d8}
:root[data-theme=dark] .cv{--mc:#fdd663}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]) .c0{--mc:#f28b82}:root:not([data-theme=light]) .c1{--mc:#8ab4f8}:root:not([data-theme=light]) .c2{--mc:#81c995}
:root:not([data-theme=light]) .c3{--mc:#d7aefb}:root:not([data-theme=light]) .c4{--mc:#a7c0cd}:root:not([data-theme=light]) .c5{--mc:#78d9cb}:root:not([data-theme=light]) .c6{--mc:#f48fb1}
:root:not([data-theme=light]) .c7{--mc:#d7b8a8}:root:not([data-theme=light]) .c8{--mc:#9fa8da}:root:not([data-theme=light]) .c9{--mc:#c5d36b}:root:not([data-theme=light]) .c10{--mc:#80deea}
:root:not([data-theme=light]) .c11{--mc:#ce93d8}:root:not([data-theme=light]) .cv{--mc:#fdd663}}
.newbtn{position:absolute;left:50%;transform:translateX(-50%);bottom:96px;background:var(--accent);color:var(--accent-fg);border:0;border-radius:16px;padding:5px 14px;box-shadow:var(--shadow);font-weight:600;z-index:2}
.newbtn:hover{background:var(--accent);filter:brightness(1.1)}
.composer{padding:8px 14px 12px;padding-bottom:max(12px,env(safe-area-inset-bottom))}
.composer form{border:1px solid var(--line);border-radius:10px;background:var(--input);padding:6px 8px}
.composer form:focus-within{border-color:var(--accent)}
.composer textarea{width:100%;border:0;outline:0;resize:none;background:transparent;min-height:24px;max-height:200px;padding:4px 2px}
.composer textarea:focus-visible{outline:0}
.crow{display:flex;flex-wrap:wrap;align-items:center;gap:6px 10px;font-size:12px;color:var(--muted)}
.sstat{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.sstat.bad{color:var(--badge)}
.crow button[type=submit]{background:var(--accent);color:var(--accent-fg);border:0;font-weight:600;padding:4px 14px}
.crow button[type=submit]:disabled{opacity:.5;cursor:default}
@media (max-width:760px){
#app,body.thread-open #app{grid-template-columns:minmax(0,1fr)}
#side,#main,#thread{grid-column:1;grid-row:1}
body[data-pane=list] #main,body[data-pane=list] #thread{display:none}
body[data-pane=chat] #side,body[data-pane=chat] #thread{display:none}
body[data-pane=thread] #side,body[data-pane=thread] #main{display:none}
#thread{border-left:0}
.back{display:inline-block}
.hmembers{display:none}
.side-item{padding:9px 10px;font-size:16px}
.msg{padding-left:10px;padding-right:10px}
.newbtn{bottom:110px}
}
body.login{display:block;background:var(--side)}
body.login main{max-width:360px;margin:15vh auto;background:var(--bg);border:1px solid var(--line);border-radius:10px;padding:24px}
body.login h1{font-size:20px;margin-top:0}
body.login input{width:100%;padding:8px;margin:6px 0 12px;border:1px solid var(--line);border-radius:6px;background:var(--input)}
.err{color:var(--badge)}
` + viewObsCSS + viewTrCSS

// viewJS: textContent only (never markup from data). Live updates come over /view/ws (ws.go:
// session cookie, same Origin, CSRF token in the first frame); each pushed frame triggers a
// fetch of the messages after the last id. While the socket is down it reconnects with
// backoff 1 s -> 30 s and polls every 4 s; while it is up it polls once a minute for the
// server status. Sends with fetch and the session CSRF token from the messages response. Read
// marks and the theme live in localStorage of this origin only.
const viewJS = `"use strict";
(function () {
  var VIEWER = "viewer", DAY = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"], CAP = 20000;
  var S = { all: [], byId: new Map(), last: 0, hasOlder: false, members: [], memberCh: {}, channels: ["team", "general"], vis: { team: "private", general: "public" },
    csrf: "", pane: "c:team", thread: 0, read: {}, busy: null, loaded: false, flashAfter: Infinity, seen: {},
    rc: {}, rseq: -1, un: {}, rcOpen: {}, votes: {}, votesKey: "" };
  var RP = { on: false, playing: false, t: 0, t0: 0, t1: 0, speed: 60, n: 0, timer: 0, tick: 0 };
  var E = {}, C = {};

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function $(id) { return document.getElementById(id); }
  function tsMs(m) { if (m._ms === undefined) m._ms = Date.parse(m.ts) || 0; return m._ms; }
  function dateOf(m) { return String(m.ts).slice(0, 10); }
  function hm(m) { return String(m.ts).slice(11, 16); }
  function jstString(ms) { return new Date(ms + 9 * 3600000).toISOString().slice(0, 19).replace("T", " "); }
  // Channel visibility comes from the server (channel_info): "#" before a public name, a lock before a private one.
  function chMark(c) { return S.vis[c] === "private" ? "🔒" : "#"; }
  function chLabel(c) { return chMark(c) + c; }
  function paneOf(m) { return m.to === "all" ? "c:" + m.channel : "d:" + [m.from, m.to].sort().join("|"); }
  function pairOf(p) { return p.slice(2).split("|"); }
  function others(p) { return pairOf(p).filter(function (x) { return x !== VIEWER; }); }
  function colorClass(name) {
    if (name === VIEWER) return "cv";
    var i = S.members.indexOf(name);
    if (i < 0) { i = 0; for (var k = 0; k < name.length; k++) i = (i * 31 + name.charCodeAt(k)) >>> 0; }
    return "c" + (i % 12);
  }
  function avatar(name, small) {
    var a = el("span", "av " + colorClass(name) + (small ? " sm" : ""), (name.charAt(0) || "?").toUpperCase());
    a.title = name; a.setAttribute("aria-hidden", "true");
    return a;
  }
  function nearBottom(sc) { return sc.scrollHeight - sc.scrollTop - sc.clientHeight < 60; }
  function size(n) { return n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KB" : (n / 1048576).toFixed(1) + " MB"; }
` + viewMDJS + viewObsJS + viewTrCoreJS + viewTrJS + `

  // --- read marks (per pane, this browser only) ---
  function loadRead() {
    try { var v = JSON.parse(localStorage.getItem("a2-view-read") || "null"); if (v && typeof v === "object") S.read = v; } catch (e) {}
  }
  function saveRead() { try { localStorage.setItem("a2-view-read", JSON.stringify(S.read)); } catch (e) {} }
  function readMark(p) { var v = S.read[p]; return typeof v === "number" ? v : (S.read["*"] || 0); }

  // --- data ---
  function visible() { return RP.on ? S.all.slice(0, RP.n) : S.all; }
  function ingest(list, older) {
    var fresh = list.filter(function (m) { return !S.byId.has(m.id); });
    fresh.forEach(function (m) { S.byId.set(m.id, m); if (m.id > S.last) S.last = m.id; });
    S.all = older ? fresh.concat(S.all) : S.all.concat(fresh);
    if (S.all.length > CAP) {
      S.all.slice(0, S.all.length - CAP).forEach(function (m) { S.byId.delete(m.id); });
      S.all = S.all.slice(S.all.length - CAP);
      S.hasOlder = true;
    }
  }
  function index(list) {
    var panes = new Map(), replies = new Map();
    S.channels.forEach(function (c) { panes.set("c:" + c, { last: 0, unread: 0, fresh: false }); });
    list.forEach(function (m) {
      var p = paneOf(m), st = panes.get(p);
      if (!st) { st = { last: 0, unread: 0, fresh: false }; panes.set(p, st); }
      st.last = m.id;
      if (m.from !== VIEWER && m.id > readMark(p)) st.unread++;
      if (m.id > S.flashAfter) st.fresh = true;
      if (m.reply_to) {
        var r = replies.get(m.reply_to);
        if (!r) { r = []; replies.set(m.reply_to, r); }
        r.push(m);
      }
    });
    return { panes: panes, replies: replies };
  }
  function getJSON(url) {
    return fetch(url, { credentials: "same-origin", cache: "no-store" }).then(function (r) {
      if (r.status === 401) { window.location.href = "/view"; throw new Error("logged out"); }
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    });
  }

  // --- sidebar and header ---
  function sideItem(p, st) {
    var li = el("li"), b = el("button", "side-item");
    b.type = "button";
    if (p === S.pane) b.classList.add("on");
    if (st.unread > 0 && p !== S.pane) b.classList.add("unread");
    if (RP.on && st.fresh) b.classList.add("pulse");
    if (p.charAt(0) === "c") {
      b.appendChild(el("span", "hash", chMark(p.slice(2))));
      b.appendChild(el("span", "nm", p.slice(2)));
    } else {
      pairOf(p).forEach(function (n) { b.appendChild(avatar(n, true)); });
      b.appendChild(el("span", "nm", pairOf(p).join(", ")));
    }
    if (st.unread > 0 && !RP.on) b.appendChild(el("span", "badge", st.unread > 99 ? "99+" : String(st.unread)));
    b.addEventListener("click", function () { selectPane(p); });
    li.appendChild(b);
    return li;
  }
  function renderSide(ix) {
    var ch = S.channels.map(function (c) { return sideItem("c:" + c, ix.panes.get("c:" + c)); });
    E.chans.replaceChildren.apply(E.chans, ch);
    var dms = [];
    ix.panes.forEach(function (st, p) { if (p.charAt(0) === "d") dms.push([p, st]); });
    dms.sort(function (a, b) { return b[1].last - a[1].last; });
    E.dms.replaceChildren.apply(E.dms, dms.map(function (x) { return sideItem(x[0], x[1]); }));
    if (!dms.length) E.dms.appendChild(el("li", "empty", "none yet"));
    var total = 0;
    ix.panes.forEach(function (st) { total += st.unread; });
    setTitle(total);
  }
  function memberChip(n) {
    var s = el("span", "mem " + colorClass(n));
    s.appendChild(avatar(n, true));
    s.appendChild(el("span", "mname", n));
    if (S.alive && S.alive[n] !== undefined) {
      var d = el("span", "ldot " + (S.alive[n] ? "on" : "off"));
      d.title = S.alive[n] ? "alive" : "no contact for 10 min";
      s.appendChild(d);
    }
    return s;
  }
  function renderHead() {
    var names;
    if (S.pane.charAt(0) === "c") {
      var c = S.pane.slice(2);
      E.title.textContent = chMark(c) + " " + c;
      names = S.members.filter(function (n) { return (S.memberCh[n] || []).indexOf(c) >= 0; });
    } else {
      names = pairOf(S.pane);
      E.title.textContent = names.join(" ↔ ");
    }
    E.members.replaceChildren.apply(E.members, names.map(memberChip));
  }

  // --- messages ---
  function daySep(d) {
    var li = el("li", "day"), label = d;
    var today = jstString(Date.now()).slice(0, 10), yday = jstString(Date.now() - 86400000).slice(0, 10);
    if (d === today) label = "Today"; else if (d === yday) label = "Yesterday";
    else label = d + " (" + DAY[new Date(d + "T00:00:00Z").getUTCDay()] + ")";
    li.appendChild(el("span", "", label));
    return li;
  }
  // vote counts under a vote message; updated by the same socket wake as receipts
  function voteNode(v) {
    var parts = (v.choices || []).map(function (c) { return c + " " + ((v.counts || {})[c] || 0); });
    var head = v.open ? "\ud83d\uddf3 open until " + String(v.closes).slice(5, 16).replace("T", " ") : "\ud83d\uddf3 closed: " + v.result + (v.winner ? " (" + v.winner + ")" : "");
    var d = el("div", "vote" + (v.open ? "" : v.passed ? " pass" : " fail"), head + " \u00b7 " + parts.join(" \u00b7 ") + " \u00b7 " + v.voters + " voted");
    d.title = v.rule + ((v.ballots || []).length ? "\n" + v.ballots.map(function (b) { return b.name + ": " + b.choice; }).join("\n") : "");
    return d;
  }
  function msgNode(m, grouped, replies, inThread, showChannel, found) {
    var li = el("li", "msg" + (grouped ? " cont" : ""));
    li.setAttribute("data-id", String(m.id));
    if (m.id > S.flashAfter) li.classList.add("fresh");
    if (m.id === S.sel) li.classList.add("sel");
    if (m.id === S.hl) li.classList.add("hl");
    var gut = el("div", "gut");
    if (grouped) { var tt = el("time", "hov", hm(m)); tt.title = m.ts; gut.appendChild(tt); }
    else gut.appendChild(avatar(m.from));
    var body = el("div", "body");
    if (!grouped) {
      var h = el("div", "mh");
      h.appendChild(el("span", "who " + colorClass(m.from), m.from));
      if (m.to !== "all") h.appendChild(el("span", "to", "→ " + m.to));
      if (showChannel) h.appendChild(el("span", "chip", chLabel(m.channel)));
      var t = el("time", "ts", hm(m)); t.title = m.ts + " (JST)";
      h.appendChild(t);
      var mid = el("a", "mid", "#" + m.id);
      mid.href = "#m=" + m.id; mid.title = "Link to this message";
      h.appendChild(mid);
      body.appendChild(h);
    }
    if (m.reply_to && !inThread) {
      var it = el("button", "inthread", "↳ reply in thread #" + m.reply_to);
      it.type = "button";
      it.addEventListener("click", function () { openThread(m.reply_to); });
      body.appendChild(it);
    }
    if (m.file_id) {
      var f = el("div", "file");
      f.appendChild(el("span", "", "📎"));
      var a = el("a", "", m.file ? m.file.name : m.file_id);
      a.href = "/view/files/" + encodeURIComponent(m.file_id);
      a.rel = "noreferrer";
      f.appendChild(a);
      if (m.file) f.appendChild(el("span", "fmeta", m.file.mime + " · " + size(m.file.size)));
      body.appendChild(f);
    } else {
      if (!m._md) m._md = mdRender(m.text); // Markdown, built once per message
      body.appendChild(trBody(m)); // translated when a target language is on viewtr.go)
      if (S.votes[m.id] && !RP.on) body.appendChild(voteNode(S.votes[m.id]));
    }
    // unread-count badge: recipients not delivered yet; none at 0 or without a count (old
    // message, replay). Hover shows the list (title), a click/tap opens it under the message.
    var rcs = RP.on ? null : S.rc[m.id], un = RP.on ? undefined : S.un[m.id], badge = null;
    if (rcs && rcs.length && typeof un === "number" && un > 0) {
      badge = el("button", "unr" + (S.rcOpen[m.id] ? " open" : ""), String(un));
      badge.type = "button";
      badge.setAttribute("aria-label", un + " not delivered yet");
      badge.title = un + " / " + rcs.length + " not delivered yet\n" + rcs.map(function (r) {
        return r.status === "delivered" ? "✓ " + r.name + " " + String(r.delivered).slice(11, 16) : "· " + r.name;
      }).join("\n");
      badge.addEventListener("click", function (ev) {
        ev.stopPropagation();
        if (S.rcOpen[m.id]) delete S.rcOpen[m.id]; else S.rcOpen[m.id] = true;
        S.flashAfter = Infinity;
        render(false);
      });
    }
    if (badge && S.rcOpen[m.id]) {
      var rr = el("div", "rcpt");
      rcs.forEach(function (r) {
        var s;
        if (r.status === "delivered") {
          s = el("span", "rc ok", "✓ " + r.name + " " + String(r.delivered).slice(11, 16));
          s.title = "delivered " + String(r.delivered).slice(0, 19).replace("T", " ") + " JST (fetched by the client, not proof that it was read)";
        } else if (r.status === "pending") {
          s = el("span", "rc no", "· " + r.name);
          s.title = "not fetched yet";
        } else {
          s = el("span", "rc no", "? " + r.name);
          s.title = "no record (message older than delivery receipts)";
        }
        rr.appendChild(s);
      });
      body.appendChild(rr);
    }
    var rs = inThread ? null : replies.get(m.id);
    if (rs && rs.length) {
      var rb = el("button", "replies");
      rb.type = "button";
      var seen = [];
      rs.forEach(function (r) { if (seen.indexOf(r.from) < 0 && seen.length < 4) { seen.push(r.from); rb.appendChild(avatar(r.from, true)); } });
      rb.appendChild(el("b", "", rs.length + (rs.length === 1 ? " reply" : " replies")));
      rb.appendChild(el("span", "last", "last " + hm(rs[rs.length - 1])));
      rb.addEventListener("click", function () { openThread(m.id); });
      body.appendChild(rb);
    }
    li.appendChild(gut);
    li.appendChild(body);
    if (badge) { var bw = el("div", "unrw"); bw.appendChild(badge); li.appendChild(bw); }
    if (!RP.on) {
      var acts = el("div", "acts");
      var act = function (label, title, fn) {
        var b = el("button", "", label);
        b.type = "button"; b.title = title;
        b.addEventListener("click", function (ev) { ev.stopPropagation(); fn(); });
        acts.appendChild(b);
      };
      if (found) act("\u2197 Show", "Show where it was posted", function () { reveal(m.id, 0); });
      if (!inThread) act("\u21a9 Reply", "Reply in thread", function () { openThread(m.reply_to || m.id); });
      act("\ud83d\udd17", "Copy a link to this message", function () { copyLink(m.id); });
      li.appendChild(acts);
    }
    return li;
  }
  function buildList(msgs, replies, inThread, showChannel, lastDate, found) {
    var out = [], prev = null;
    msgs.forEach(function (m) {
      var d = prev ? dateOf(prev) : lastDate;
      if (d !== dateOf(m)) { out.push(daySep(dateOf(m))); prev = null; }
      var grouped = !found && !!prev && prev.from === m.from && prev.to === m.to && tsMs(m) - tsMs(prev) < 300000 &&
        (inThread || (!prev.reply_to && !m.reply_to && !replies.has(prev.id)));
      out.push(msgNode(m, grouped, replies, inThread, showChannel, found));
      prev = m;
    });
    return out;
  }
  function paneMsgs(list) {
    return list.filter(function (m) {
      if (paneOf(m) !== S.pane) return false;
      var root = m.reply_to ? S.byId.get(m.reply_to) : null;
      return !(root && paneOf(root) === S.pane);
    });
  }
  function renderMain(list, ix, force) {
    var sc = E.scroll, stick = force || nearBottom(sc);
    if (Q.q) { renderSearch(list, ix, stick); return; }
    var msgs = paneMsgs(list);
    var items = buildList(msgs, ix.replies, false, S.pane.charAt(0) === "d");
    if (!items.length) items.push(el("li", "empty", RP.on ? "Nothing here yet at this point of the replay." : "No messages yet."));
    E.list.replaceChildren.apply(E.list, items);
    E.older.hidden = !S.hasOlder || RP.on;
    var top = msgs.length ? msgs[msgs.length - 1].id : 0, prev = S.seen[S.pane] || 0;
    if (stick) { sc.scrollTop = sc.scrollHeight; E.newbtn.hidden = true; }
    else if (prev && top > prev) E.newbtn.hidden = false;
    S.seen[S.pane] = top;
  }
  function renderThread(list, ix, force) {
    var root = S.byId.get(S.thread);
    var lastId = list.length ? list[list.length - 1].id : 0;
    if (!root || root.id > lastId) {
      E.thList.replaceChildren(el("li", "empty", root ? "This thread starts later in the replay." : "Message not loaded."));
      E.thSub.replaceChildren();
      return;
    }
    var sc = E.thScroll, stick = force || nearBottom(sc);
    var reps = ix.replies.get(root.id) || [];
    var items = buildList([root], ix.replies, true, true);
    items.push(el("li", "thsep", reps.length + (reps.length === 1 ? " reply" : " replies")));
    items = items.concat(buildList(reps, ix.replies, true, false, dateOf(root)));
    E.thList.replaceChildren.apply(E.thList, items);
    var sub = [el("span", "", chLabel(root.channel))];
    if (root.to !== "all") sub.push(el("span", "", root.from + " ↔ " + root.to));
    E.thSub.replaceChildren.apply(E.thSub, sub);
    if (stick) sc.scrollTop = sc.scrollHeight;
  }
  function markRead(ix) {
    if (RP.on || document.hidden || Q.q || H.on || !nearBottom(E.scroll)) return;
    var st = ix.panes.get(S.pane);
    if (st && st.last > readMark(S.pane)) { S.read[S.pane] = st.last; saveRead(); }
    if (st) st.unread = 0;
    E.newbtn.hidden = true;
  }
  function render(force) {
    var list = visible(), ix = index(list);
    renderHead();
    if (H.on) renderHealth(list); else renderMain(list, ix, force);
    markRead(ix);
    renderSide(ix);
    if (S.thread) renderThread(list, ix, force);
  }
  function refreshSide() { var ix = index(visible()); markRead(ix); renderSide(ix); }

  // --- navigation ---
  function selectPane(p) {
    S.pane = p;
    S.flashAfter = Infinity;
    closeThread(true);
    document.body.setAttribute("data-pane", "chat");
    fillComposer("main");
    render(true);
  }
  function openThread(id) {
    S.thread = id;
    E.thread.hidden = false;
    document.body.classList.add("thread-open");
    document.body.setAttribute("data-pane", "thread");
    fillComposer("thread");
    var list = visible();
    renderThread(list, index(list), true);
  }
  function closeThread(quiet) {
    S.thread = 0;
    E.thread.hidden = true;
    document.body.classList.remove("thread-open");
    if (!quiet) document.body.setAttribute("data-pane", "chat");
  }

  // --- composers ---
  function composerCtx(kind) {
    if (kind === "thread") {
      var r = S.byId.get(S.thread);
      if (!r) return null;
      return { channels: [r.channel], tos: r.to === "all" ? ["all"] : others("d:" + [r.from, r.to].sort().join("|")),
        replyTo: r.id, label: "Reply in thread" };
    }
    if (S.pane.charAt(0) === "c") {
      var c = S.pane.slice(2);
      return { channels: [c], tos: ["all"], replyTo: 0, label: "Message " + chLabel(c) };
    }
    var o = others(S.pane);
    var chs = S.channels.filter(function (ch) { return o.every(function (x) { return (S.memberCh[x] || []).indexOf(ch) >= 0; }); });
    var lastCh = "";
    for (var i = S.all.length - 1; i >= 0; i--) if (paneOf(S.all[i]) === S.pane) { lastCh = S.all[i].channel; break; }
    if (lastCh && chs.indexOf(lastCh) > 0) chs = [lastCh].concat(chs.filter(function (x) { return x !== lastCh; }));
    var label = o.length > 1 ? "Message " + o.join(" or ") + " (sent as your own DM)" : "Message " + o.join("");
    return { channels: chs, tos: o, replyTo: 0, label: label };
  }
  function fillSelect(sel, values, lab) {
    var keep = sel.value;
    sel.replaceChildren.apply(sel, values.map(function (v) { var o = el("option", "", lab ? lab(v) : v); o.value = v; return o; }));
    if (values.indexOf(keep) >= 0) sel.value = keep;
  }
  function fillComposer(kind) {
    var c = C[kind], ctx = composerCtx(kind);
    var chs = ctx ? ctx.channels : [], tos = ctx ? ctx.tos : [];
    fillSelect(c.ch, chs, chLabel);
    if (kind === "main" && chs.length) c.ch.value = chs[0];
    fillSelect(c.to, tos);
    c.chW.hidden = chs.length < 2;
    c.toW.hidden = tos.length < 2;
    var off = RP.on || !ctx || !chs.length || !tos.length;
    c.text.disabled = off; c.btn.disabled = off;
    c.text.placeholder = RP.on ? "Replay is on; writing is paused" : ctx ? ctx.label : "";
    say(c, "", false);
  }
  function fillAll() { fillComposer("main"); if (S.thread) fillComposer("thread"); }
  function say(c, msg, bad) { c.status.textContent = msg; c.status.className = "sstat" + (bad ? " bad" : ""); }
  function grow(ta) { ta.style.height = "auto"; ta.style.height = Math.min(ta.scrollHeight, 200) + "px"; }
  function send(kind, ev) {
    ev.preventDefault();
    var c = C[kind], ctx = composerCtx(kind);
    if (!S.csrf || !ctx) { say(c, "not ready, wait a moment", true); return; }
    if (!c.text.value.trim()) return;
    var body = new URLSearchParams();
    body.set("csrf", S.csrf); body.set("channel", c.ch.value); body.set("to", c.to.value); body.set("text", c.text.value);
    if (ctx.replyTo) body.set("reply_to", String(ctx.replyTo));
    c.btn.disabled = true;
    fetch("/view/send", { method: "POST", credentials: "same-origin", cache: "no-store", body: body })
      .then(function (r) {
        if (r.status === 401) { window.location.href = "/view"; throw new Error("logged out"); }
        return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ("HTTP " + r.status)); return d; });
      })
      .then(function (m) {
        c.text.value = ""; grow(c.text);
        say(c, "sent #" + m.id, false);
        return poll().then(function () {
          if (kind === "main" && !m.reply_to && paneOf(m) !== S.pane) selectPane(paneOf(m));
          else { if (kind === "main") E.scroll.scrollTop = E.scroll.scrollHeight; render(kind === "main"); }
        });
      })
      .catch(function (e) { say(c, "not sent: " + e.message, true); })
      .then(function () { c.btn.disabled = RP.on; });
  }

  // --- server version and member rename ---
  function shortTs(s) { return s ? String(s).replace("T", " ").replace(/:\d\d\+09:00$/, "") : "-"; }
  function showServer(sv) {
    var lu = sv.last_update;
    var txt = "server " + (sv.build || "?") + " · updated " + shortTs(lu ? lu.at : sv.started);
    $("srv").textContent = txt;
    $("srv").title = "build " + (sv.build || "?") + ", built " + shortTs(sv.built) + ", running since " + shortTs(sv.started) +
      (lu ? ", last self-update " + shortTs(lu.at) + " (" + lu.from + " -> " + lu.to + ")" : "");
  }
  function fillRename() {
    var sel = $("rn-old");
    while (sel.firstChild) sel.removeChild(sel.firstChild);
    (S.members || []).forEach(function (m) { var o = document.createElement("option"); o.value = m; o.textContent = m; sel.appendChild(o); });
  }
  function rename(ev) {
    ev.preventDefault();
    var st = $("rn-status"), nw = $("rn-new").value.trim(), old = $("rn-old").value;
    if (!S.csrf) { st.textContent = "not ready"; return; }
    if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(nw) || nw.length > 40) { st.textContent = "lowercase, digits, single hyphens, max 40"; st.className = "sstat bad"; return; }
    var body = new URLSearchParams();
    body.set("csrf", S.csrf); body.set("old", old); body.set("new", nw);
    $("rn-btn").disabled = true;
    fetch("/view/rename", { method: "POST", credentials: "same-origin", cache: "no-store", body: body })
      .then(function (r) { return r.json().then(function (d) { if (!r.ok) throw new Error(d.error || ("HTTP " + r.status)); return d; }); })
      .then(function () { window.location.reload(); })
      .catch(function (e) { st.textContent = "not renamed: " + e.message; st.className = "sstat bad"; })
      .then(function () { $("rn-btn").disabled = false; });
  }

  // --- live updates (WebSocket) and polling ---
  var WS = { up: false, delay: 1000 };
  function pollSoon() { if (S.busy) S.again = true; else poll(); }
  function live() {
    if (!S.csrf) { setTimeout(live, WS.delay); return; }
    var s;
    try { s = new WebSocket("wss://" + window.location.host + "/view/ws"); }
    catch (e) { setTimeout(live, WS.delay); WS.delay = Math.min(WS.delay * 2, 30000); return; }
    s.onopen = function () {
      WS.up = true; WS.delay = 1000;
      s.send(JSON.stringify({ csrf: S.csrf, since: S.last }));
      pollSoon();
    };
    s.onmessage = pollSoon;
    s.onclose = function () {
      WS.up = false;
      E.status.textContent = "live: reconnecting";
      setTimeout(live, WS.delay);
      WS.delay = Math.min(WS.delay * 2, 30000);
    };
  }
  function poll() {
    if (S.busy) return S.busy;
    S.busy = getJSON("/view/api/messages?since=" + S.last + (S.rseq >= 0 ? "&rseq=" + S.rseq : ""))
      .then(function (d) {
        if (d.csrf) S.csrf = d.csrf;
        var got = d.messages || [], before = S.last, rcNew = takeReceipts(d), vNew = takeVotes(d);
        if (!S.loaded) {
          S.members = d.members || [];
          S.memberCh = d.member_channels || {};
          if (d.channels && d.channels.length) S.channels = d.channels;
          if (d.channel_info) d.channel_info.forEach(function (ci) { S.vis[ci.name] = ci.visibility; });
          fillRename();
          S.hasOlder = !!d.truncated;
        }
        if (d.alive) {
          var ak = JSON.stringify(d.alive), achg = ak !== S.aliveKey;
          S.alive = d.alive; S.aliveKey = ak;
          if (achg && S.loaded) renderHead();
        }
        if (got.length) ingest(got, false);
        if (!S.loaded) {
          S.loaded = true;
          if (S.read["*"] === undefined) { S.read["*"] = S.last; saveRead(); }
          fillAll();
          render(true);
          openLinked();
        } else if (got.length) {
          noticeNew(got);
          if (RP.on) { RP.t1 = tsMs(S.all[S.all.length - 1]); rpUI(); }
          else { S.flashAfter = before; render(false); }
        } else if ((rcNew || vNew) && !RP.on) { S.flashAfter = Infinity; render(false); }
        S.polled = Date.now();
        E.status.textContent = "updated " + new Date().toLocaleTimeString() + (WS.up ? " · live" : "");
        if (d.server) showServer(d.server);
      })
      .catch(function (e) { E.status.textContent = "error: " + e.message; })
      .then(function () { S.busy = null; if (S.again) { S.again = false; poll(); } });
    return S.busy;
  }
  // receipts: {id: [{name, status, delivered}]}; rseq is the server's change counter
  function takeReceipts(d) {
    var n = 0;
    if (d.receipts) Object.keys(d.receipts).forEach(function (k) { S.rc[k] = d.receipts[k]; n++; });
    takeBadges(d);
    if (typeof d.rseq === "number") S.rseq = d.rseq;
    return n > 0;
  }
  // votes: {message id: vote}; true when anything changed (counts, open/closed)
  function takeVotes(d) {
    if (!d.votes) return false;
    var k = JSON.stringify(d.votes);
    if (k === S.votesKey) return false;
    S.votes = d.votes; S.votesKey = k;
    return true;
  }
  // undelivered: {id: n} badge counts, only for messages the delivery log covers
  function takeBadges(d) {
    if (d.undelivered) Object.keys(d.undelivered).forEach(function (k) { S.un[k] = d.undelivered[k]; });
  }
  function loadOlder() {
    if (!S.all.length) return Promise.resolve();
    E.older.disabled = true;
    return getJSON("/view/api/messages?before=" + S.all[0].id)
      .then(function (d) {
        var sc = E.scroll, h0 = sc.scrollHeight, top0 = sc.scrollTop;
        if (d.receipts) Object.keys(d.receipts).forEach(function (k) { S.rc[k] = d.receipts[k]; });
        takeBadges(d);
        ingest(d.messages || [], true);
        S.hasOlder = !!d.truncated;
        S.flashAfter = Infinity;
        render(false);
        sc.scrollTop = top0 + (sc.scrollHeight - h0);
      })
      .catch(function (e) { E.status.textContent = "error: " + e.message; })
      .then(function () { E.older.disabled = false; });
  }

  // --- replay ---
  function countUpTo(t) {
    var lo = 0, hi = S.all.length;
    while (lo < hi) { var mid = (lo + hi) >> 1; if (tsMs(S.all[mid]) <= t) lo = mid + 1; else hi = mid; }
    return lo;
  }
  function rpUI() {
    var span = Math.max(1, RP.t1 - RP.t0);
    E.rpPos.value = String(Math.round((RP.t - RP.t0) / span * 1000));
    E.rpClock.textContent = jstString(RP.t) + " JST · " + RP.n + "/" + S.all.length;
  }
  function rpSet(t, force) {
    RP.t = Math.max(RP.t0, Math.min(RP.t1, t));
    var n = countUpTo(RP.t);
    if (n !== RP.n) {
      S.flashAfter = n > RP.n && RP.n > 0 ? S.all[RP.n - 1].id : Infinity;
      RP.n = n;
      render(force);
    }
    rpUI();
  }
  function rpEnter() {
    if (!S.all.length) return;
    RP.on = true;
    RP.t0 = tsMs(S.all[0]); RP.t1 = tsMs(S.all[S.all.length - 1]);
    RP.n = -1;
    E.replay.hidden = false;
    E.rpBtn.classList.add("on");
    fillAll();
    rpSet(RP.t0, true);
  }
  function rpExit() {
    rpPause();
    RP.on = false;
    E.replay.hidden = true;
    E.rpBtn.classList.remove("on");
    S.flashAfter = Infinity;
    fillAll();
    render(true);
  }
  function rpPause() {
    RP.playing = false;
    if (RP.timer) clearInterval(RP.timer);
    RP.timer = 0;
    E.rpPlay.textContent = "Play";
  }
  function rpStep() {
    var now = performance.now(), dt = now - RP.tick;
    RP.tick = now;
    var t = RP.t + dt * RP.speed, next = S.all[RP.n];
    if (E.rpSkip.checked && next && tsMs(next) - t > 2000 * RP.speed) t = tsMs(next) - 500 * RP.speed;
    if (t >= RP.t1) { rpSet(RP.t1, false); rpPause(); return; }
    rpSet(t, false);
  }
  function rpPlay() {
    if (RP.t >= RP.t1) rpSet(RP.t0, true);
    RP.playing = true;
    RP.tick = performance.now();
    RP.timer = setInterval(rpStep, 100);
    E.rpPlay.textContent = "Pause";
  }

  // --- theme ---
  var THEMES = ["auto", "light", "dark"];
  function applyTheme(t) {
    if (t === "light" || t === "dark") document.documentElement.setAttribute("data-theme", t);
    else { document.documentElement.removeAttribute("data-theme"); t = "auto"; }
    E.theme.textContent = t.charAt(0).toUpperCase() + t.slice(1);
    try { localStorage.setItem("a2-view-theme", t); } catch (e) {}
  }

  document.addEventListener("DOMContentLoaded", function () {
    ["chans", "dms", "status", "list", "scroll", "older", "newbtn", "thread", "theme", "replay"].forEach(function (k) { E[k] = $(k); });
    E.title = $("pane-title"); E.members = $("pane-members");
    E.thList = $("th-list"); E.thScroll = $("th-scroll"); E.thSub = $("th-sub");
    E.rpBtn = $("rp-btn"); E.rpPlay = $("rp-play"); E.rpPos = $("rp-pos"); E.rpSpeed = $("rp-speed"); E.rpSkip = $("rp-skip"); E.rpClock = $("rp-clock");
    [["main", "s", "send"], ["thread", "t", "th-send"]].forEach(function (k) {
      var c = { form: $(k[2]), text: $(k[1] + "-text"), ch: $(k[1] + "-ch"), to: $(k[1] + "-to"), btn: $(k[1] + "-btn"),
        status: $(k[1] + "-status"), chW: $(k[1] + "-ch-w"), toW: $(k[1] + "-to-w") };
      C[k[0]] = c;
      c.form.addEventListener("submit", function (ev) { send(k[0], ev); });
      c.text.addEventListener("input", function () { grow(c.text); });
      c.text.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing && ev.keyCode !== 229) { ev.preventDefault(); c.form.requestSubmit(); }
      });
    });
    var t0 = "auto";
    try { t0 = localStorage.getItem("a2-view-theme") || "auto"; } catch (e) {}
    applyTheme(t0);
    E.theme.addEventListener("click", function () {
      var cur = document.documentElement.getAttribute("data-theme") || "auto";
      applyTheme(THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length]);
    });
    $("back").addEventListener("click", function () { document.body.setAttribute("data-pane", "list"); });
    $("th-back").addEventListener("click", function () { closeThread(false); });
    $("th-close").addEventListener("click", function () { closeThread(false); });
    obsInit(); // search, health, links, keys; Escape still closes the thread
    trInit(); // translation viewtr.go)
    E.newbtn.addEventListener("click", function () { E.scroll.scrollTop = E.scroll.scrollHeight; E.newbtn.hidden = true; refreshSide(); });
    var pending = false;
    E.scroll.addEventListener("scroll", function () {
      if (pending) return;
      pending = true;
      requestAnimationFrame(function () { pending = false; if (nearBottom(E.scroll)) { E.newbtn.hidden = true; if (!RP.on) refreshSide(); } });
    });
    document.addEventListener("visibilitychange", function () { if (!document.hidden) { NOTICE.n = 0; setTitle(); poll(); refreshSide(); } });
    E.older.addEventListener("click", loadOlder);
    E.rpBtn.addEventListener("click", function () { if (RP.on) rpExit(); else rpEnter(); });
    $("rp-exit").addEventListener("click", rpExit);
    E.rpPlay.addEventListener("click", function () { if (RP.playing) rpPause(); else rpPlay(); });
    E.rpSpeed.addEventListener("change", function () { RP.speed = Number(E.rpSpeed.value) || 1; });
    RP.speed = Number(E.rpSpeed.value) || 60;
    E.rpPos.addEventListener("input", function () {
      rpSet(RP.t0 + (RP.t1 - RP.t0) * Number(E.rpPos.value) / 1000, true);
    });
    $("rn").addEventListener("submit", rename);
    loadRead();
    poll().then(live);
    setInterval(function () { if (!WS.up || Date.now() - S.polled > 60000) poll(); }, 4000);
  });
})();
`
