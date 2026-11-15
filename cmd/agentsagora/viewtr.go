package main

// Message translation in the viewer UI, adapted from an earlier observer page
// (index.html "Translation"): Chrome's built-in Translator API, on the device, nothing is sent to a
// server. Chrome offers the API on localhost (or a real domain) in desktop Chrome 138+, so
// on the server host the view is opened at https://localhost:18765/view (viewlocal.go); on the LAN
// address and on phones the page shows once, in the sidebar, which requirement is missing.
//
// How it works: the message list is rebuilt on each render, so a translation is kept per
// message (m._tr = {target, node}) next to the Markdown node (m._md) and used by msgNode
// (viewui.go trBody). Message bodies are translated when they come near the screen
// (IntersectionObserver), one at a time, line by line; the source language is guessed per
// line (kana: ja, Hangul: ko, Latin letters: en). Code blocks, inline code and links are not
// translated, nor are lines that look like code. Translations persist in IndexedDB
// ("a2-view-tl"): keys are SHA-256 hashes of "target|line", only the translated text is
// stored, the newest viewTrMax lines are kept. Targets: original (default), 한국어 (Korean), 日本語 (Japanese),
// English; the choice is kept in localStorage of this origin.
//
// Security model unchanged: textContent / text nodes only, no innerHTML, no inline script
// (served inside /view/app.js, CSP script-src 'self'), no external resource.

// viewTrCoreJS: the parts that do not touch the page (tested under node, viewtr_test.go).
const viewTrCoreJS = `
  // --- translation ---
  var TR_MAX = 5000, TR_SKIP = { PRE: 1, CODE: 1, A: 1 };
  // source language of one line (kana -> ja, Hangul -> ko, Latin letters -> en)
  function trSrc(line) {
    if (/[\u3040-\u30FF]/.test(line)) return "ja";
    if (/[\uAC00-\uD7AF]/.test(line)) return "ko";
    if (/[A-Za-z]{3}/.test(line)) return "en";
    return null;
  }
  // lines that look like code or a command stay as they are (code blocks are skipped as nodes)
  function trCodeLike(l) { return /^\s*(\$ |#!|\/\/|[{}[\]])|[;{]\s*$/.test(l); }
  // text nodes under n, without code blocks, inline code, links and translate="no"
  function trTexts(n, out) {
    out = out || [];
    var kids = n.childNodes || [];
    for (var i = 0; i < kids.length; i++) {
      var k = kids[i];
      if (k.nodeType === 3) out.push(k);
      else if (k.nodeType === 1 && !TR_SKIP[String(k.tagName).toUpperCase()] && !(k.getAttribute && k.getAttribute("translate") === "no")) trTexts(k, out);
    }
    return out;
  }
  // trNode translates the text nodes of node in place, line by line, with line(text, target).
  // Returns true when something changed, false when nothing did, null when stillOn() turned
  // false on the way (the target changed: the half-done node is thrown away).
  async function trNode(node, target, line, stillOn) {
    var changed = false, ts = trTexts(node);
    for (var i = 0; i < ts.length; i++) {
      var lines = ts[i].data.split("\n"), outs = [];
      for (var j = 0; j < lines.length; j++) {
        if (!stillOn()) return null;
        var l = lines[j];
        outs.push(l.trim() && !trCodeLike(l) ? await line(l, target) : l);
      }
      if (!stillOn()) return null;
      var s = outs.join("\n");
      if (s !== ts[i].data) { ts[i].data = s; changed = true; }
    }
    return changed;
  }
  var TR = { target: "", api: false, translators: new Map(), cache: new Map(), chain: Promise.resolve(), pending: 0,
    io: null, watched: new Set(), busy: new Set(), db: null, sweep: 0, status: function () {} };
  function trTranslator(src, target) {
    var key = src + ">" + target;
    if (!TR.translators.has(key)) {
      var p = Translator.create({
        sourceLanguage: src, targetLanguage: target,
        monitor: function (m) { m.addEventListener("downloadprogress", function (e) { TR.status("Preparing the translation model " + Math.round(e.loaded * 100) + "%"); }); }
      });
      p.catch(function () { TR.translators.delete(key); });
      TR.translators.set(key, p);
    }
    return TR.translators.get(key);
  }
  // IndexedDB cache (FIFO, newest TR_MAX lines). Without IndexedDB or crypto.subtle only the
  // in-memory cache is used.
  function trDB() {
    if (TR.db) return TR.db;
    TR.db = new Promise(function (resolve) {
      if (!globalThis.indexedDB || !(globalThis.crypto && crypto.subtle)) { resolve(null); return; }
      try {
        var req = indexedDB.open("a2-view-tl", 1);
        req.onupgradeneeded = function () { req.result.createObjectStore("tl", { autoIncrement: true }).createIndex("k", "k", { unique: true }); };
        req.onsuccess = function () { resolve(req.result); };
        req.onerror = req.onblocked = function () { resolve(null); };
      } catch (e) { resolve(null); }
    });
    return TR.db;
  }
  function trDone(req) { return new Promise(function (resolve, reject) { req.onsuccess = function () { resolve(req.result); }; req.onerror = function () { reject(req.error); }; }); }
  async function trHash(s) {
    var d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
    return Array.from(new Uint8Array(d), function (b) { return b.toString(16).padStart(2, "0"); }).join("");
  }
  async function trLoad(k) {
    try {
      var db = await trDB();
      if (!db) return null;
      var hit = await trDone(db.transaction("tl").objectStore("tl").index("k").get(k));
      return hit ? hit.v : null;
    } catch (e) { return null; }
  }
  async function trSave(k, v) {
    try {
      var db = await trDB();
      if (!db) return;
      var store = db.transaction("tl", "readwrite").objectStore("tl");
      if (await trDone(store.index("k").getKey(k)) !== undefined) return;
      await trDone(store.add({ k: k, v: v }));
      var extra = (await trDone(store.count())) - TR_MAX;
      if (extra <= 0) return;
      // primary keys grow with each add, so ascending order is oldest first
      var cur = store.openCursor();
      cur.onsuccess = function () { var c = cur.result; if (c && extra-- > 0) { c.delete(); c.continue(); } };
    } catch (e) {}
  }
  // trLine: one line, keeping its leading and trailing spaces; same-language lines unchanged
  async function trLine(line, target) {
    var m = line.match(/^(\s*)([\s\S]*?)(\s*)$/), core = m[2], src = trSrc(core);
    if (!core || !src || src === target) return line;
    var key = target + "|" + core;
    if (!TR.cache.has(key)) {
      if (TR.cache.size > TR_MAX * 2) TR.cache.clear();
      var p = (async function () {
        var k = await trHash(key).catch(function () { return null; });
        var hit = k && (await trLoad(k));
        if (hit != null) return hit;
        var out = await (await trTranslator(src, target)).translate(core);
        if (k) trSave(k, out);
        return out;
      })();
      p.catch(function () { TR.cache.delete(key); }); // a failed line is tried again later
      TR.cache.set(key, p);
    }
    return m[1] + (await TR.cache.get(key)) + m[3];
  }
`

// viewTrJS: the page side. Uses the closure of app.js (S, E, el, $, render).
const viewTrJS = `
  function trStatus(s) {
    E.trMsg.textContent = s; E.trMsg.title = s;
    E.trNote.hidden = !s;
  }
  // Why the Translator API is missing, so the note says what to change. Phones first: there
  // the LAN address is right and the API is never offered.
  function trMissing() {
    var ua = navigator.userAgentData, port = location.port ? ":" + location.port : "";
    if ((ua && ua.mobile) || /Android|iPhone|iPad|Mobile/.test(navigator.userAgent)) return "Translation needs desktop Chrome 138+ (not on phones).";
    if (!/^(localhost|127\.0\.0\.1|\[::1\])$/.test(location.hostname)) return "Translation: on the server PC, open https://localhost" + port + "/view in Chrome 138+.";
    if (!isSecureContext) return "Translation needs a secure page (https or localhost).";
    var chrome = ua && ua.brands && ua.brands.find(function (b) { return b.brand === "Google Chrome"; });
    if (!chrome) return "Translation needs Google Chrome 138+ on a computer.";
    if (Number(chrome.version) < 138) return "Chrome " + chrome.version + " has no built-in translation (update to 138+).";
    return "Built-in translation is off in this Chrome (a policy may block it).";
  }
  // trBody: the message body for msgNode, translated when a translation for the target exists;
  // otherwise the original, watched so it is translated when it comes near the screen.
  function trBody(m) {
    var t = TR.target;
    if (!t || !TR.api) return m._md.cloneNode(true);
    if (m._tr && m._tr.target === t) return m._tr.node.cloneNode(true);
    var n = m._md.cloneNode(true);
    n.setAttribute("data-trid", String(m.id));
    TR.io.observe(n);
    TR.watched.add(n);
    if (!TR.sweep) TR.sweep = setTimeout(trSweep, 1000);
    return n;
  }
  // nodes of an earlier render are off the page: stop watching them
  function trSweep() {
    TR.sweep = 0;
    TR.watched.forEach(function (x) { if (!x.isConnected) { TR.io.unobserve(x); TR.watched.delete(x); } });
  }
  function trSeen(entries) {
    entries.forEach(function (e) { if (e.isIntersecting) trQueue(Number(e.target.getAttribute("data-trid"))); });
  }
  function trQueue(id) {
    var m = S.byId.get(id), t = TR.target, key = t + ":" + id;
    if (!m || !m._md || !t || (m._tr && m._tr.target === t) || TR.busy.has(key)) return;
    TR.busy.add(key);
    TR.pending++;
    trStatus("Translating…");
    TR.chain = TR.chain.then(function () { return trMsg(m, t); }).catch(function (e) {
      // the model download needs a click (user activation)
      if (TR.target === t) trStatus(e && e.name === "NotAllowedError" ? "Choose the language again to download the translation model (it needs a click)." : "Translation failed (" + ((e && e.message) || e) + ")");
    }).finally(function () {
      TR.busy.delete(key);
      if (--TR.pending === 0 && E.trMsg.textContent === "Translating…") trStatus("");
    });
  }
  async function trMsg(m, t) {
    var node = m._md.cloneNode(true);
    var ch = await trNode(node, t, trLine, function () { return TR.target === t; });
    if (ch === null) return;
    if (ch) { node.classList.add("trd"); node.title = "Translated on this device (Chrome). Choose Original to see the text as sent."; }
    m._tr = { target: t, node: node };
    document.querySelectorAll('[data-trid="' + m.id + '"]').forEach(function (x) {
      TR.io.unobserve(x); TR.watched.delete(x);
      x.replaceWith(node.cloneNode(true));
    });
  }
  function trSet(t, fromUser) {
    TR.target = t;
    try { localStorage.setItem("a2-view-tl", t); } catch (e) {}
    trStatus("");
    if (t) {
      // start preparing the models right after the click (downloads need a user gesture)
      if (fromUser) ["en", "ja", "ko"].forEach(function (src) { if (src !== t) trTranslator(src, t).catch(function () {}); });
      var probe = t === "en" ? "ja" : "en";
      Promise.race([
        Translator.availability({ sourceLanguage: probe, targetLanguage: t }).catch(function () { return "unavailable"; }),
        new Promise(function (r) { setTimeout(function () { r("timeout"); }, 10000); })
      ]).then(function (a) {
        if (TR.target !== t) return;
        if (a === "unavailable") trStatus("This Chrome cannot translate into this language.");
        else if (a === "timeout" && TR.pending) trStatus("Translation is not responding (open the page in your regular Chrome).");
        else if (a === "downloadable" && !fromUser) trStatus("Choose the language again to download the translation model (it needs a click).");
      });
    }
    if (fromUser) render(false);
  }
  function trInit() {
    E.trSel = $("tl"); E.trNote = $("tl-note"); E.trMsg = $("tl-msg");
    TR.status = trStatus;
    TR.api = typeof Translator !== "undefined" && typeof IntersectionObserver !== "undefined";
    if (!TR.api) {
      // one small note in the sidebar, not per message; hidden for good once closed (per reason)
      var why = trMissing(), off = "";
      try { off = localStorage.getItem("a2-view-tl-note") || ""; } catch (e) {}
      if (off !== why) trStatus(why);
      $("tl-x").addEventListener("click", function () {
        try { localStorage.setItem("a2-view-tl-note", why); } catch (e) {}
        E.trNote.hidden = true;
      });
      return;
    }
    $("tl-x").addEventListener("click", function () { E.trNote.hidden = true; });
    TR.io = new IntersectionObserver(trSeen, { rootMargin: "600px 0px" });
    var t = "en";
    try { var v = localStorage.getItem("a2-view-tl"); if (v !== null && /^(|ko|ja|en)$/.test(v)) t = v; } catch (e) {}
    E.trSel.value = t;
    E.trSel.hidden = false;
    E.trSel.addEventListener("change", function () { trSet(E.trSel.value, true); });
    trSet(t, false);
  }
`

// viewTrCSS is appended to viewCSS.
const viewTrCSS = `.side-tools{display:flex;align-items:center;gap:6px}
.tlsel{font-size:12px;max-width:96px;color:var(--fg)}
.tlnote{display:flex;align-items:flex-start;gap:4px;margin:0 12px 6px;padding:4px 6px;border:1px solid var(--line);border-radius:6px;font-size:11.5px;line-height:1.35;color:var(--muted);background:var(--bg)}
.tlnote #tl-msg{flex:1;min-width:0;overflow-wrap:anywhere}
.tlnote button{border:0;background:transparent;color:var(--muted);padding:0 2px;line-height:1}
.md.trd{border-left:2px solid var(--line);padding-left:6px}
`
