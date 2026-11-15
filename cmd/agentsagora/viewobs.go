package main

// Viewer UI features adapted from an earlier observer page: Markdown in message bodies, search
// and filters, a health panel, links to one message, a background-tab title count and
// keyboard shortcuts. Everything runs in the page; no new server route. The pieces below are
// concatenated into viewJS / viewCSS (viewui.go) and share the closure of app.js.
//
// Security model unchanged: the script still builds the DOM with createElement /
// createTextNode / textContent only (no innerHTML), no inline script or style attribute
// (CSS custom properties are set through the CSSOM, which CSP style-src 'self' allows), and
// links are made only for http(s) URLs, opened with rel="noopener noreferrer".

// viewMDJS: Markdown of message bodies (port of observer index.html md(), linkify(),
// inline(), breakLists()). Self-contained: it uses only document.createElement and
// document.createTextNode, so the Go test can run it under node with a small fake DOM.
// The Go raw string cannot hold a backtick, so the code uses \x60 for it.
const viewMDJS = `
  // --- markdown ---
  // Blocks: fenced code, pipe tables, # headings, > quotes, --- rules, lists (nested by indent,
  // also the bullet characters agents use); inline: code spans, **bold** / *bold*, _italic_,
  // ~~strike~~ / ~strike~, [label](url), <url|label> and bare URLs. Text always goes in as a
  // text node, so markup written in a message stays text.
  var MD_LI = /^(\s*)((?:[-*+]|\d{1,3}[.)])(?=\s)|[•・])\s*(\S.*)$/;
  var MD_QUOTE = /^\s*>\s?/, MD_RULE = /^[ \t]*([-*_])([ \t]*\1){2,}[ \t]*$/;
  var MD_FENCE = /^\s*(\x60\x60\x60|~~~)/, MD_TROW = /^\s*\|.*\|\s*$/, MD_TSEP = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/;
  var MD_LINK = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|<(https?:\/\/[^\s<>|]+)(?:\|([^<>\n]+))?>|https?:\/\/[^\s<>"'）)」』、。，]+/g;
  // [pattern, tag, has a prefix group]; a single marker needs a non-space just inside and no
  // letter or digit just outside (2*3*4, snake_case, 10~20 stay text)
  var MD_EMPH = [
    [/\*\*([^*\n]+)\*\*/, "strong", false],
    [/(^|[^A-Za-z0-9*])\*(?=[^\s*])([^*\n]*?[^\s*])\*(?![A-Za-z0-9*])/, "strong", true],
    [/(^|[^\p{L}\p{N}_])_(?=[^\s_])([^_\n]*?[^\s_])_(?![\p{L}\p{N}_])/u, "em", true],
    [/~~([^~\n]+)~~/, "del", false],
    [/(^|[^\p{L}\p{N}~])~(?=[^\s~])([^~\n]*?[^\s~])~(?![\p{L}\p{N}~])/u, "del", true]
  ];
  function mdEl(tag, cls) { var e = document.createElement(tag); if (cls) e.className = cls; return e; }
  function mdText(parent, s) { if (s) parent.appendChild(document.createTextNode(s)); }
  // only absolute http(s) URLs become links; anything else stays text
  function mdSafeURL(u) {
    if (!/^https?:\/\//i.test(u)) return "";
    try { var x = new URL(u); return x.protocol === "http:" || x.protocol === "https:" ? x.href : ""; } catch (e) { return ""; }
  }
  function mdLinkLabel(u) {
    try {
      var x = new URL(u), p = x.pathname.length > 1 ? x.pathname : "";
      return x.host + (p.length > 40 ? p.slice(0, 40) + "…" : p) + (x.search || x.hash ? "…" : "");
    } catch (e) { return u; }
  }
  function mdEmph(parent, s) {
    while (s) {
      var best = null;
      for (var i = 0; i < MD_EMPH.length; i++) {
        var r = MD_EMPH[i], m = r[0].exec(s);
        if (!m) continue;
        var at = m.index + (r[2] ? m[1].length : 0);
        if (!best || at < best.at) best = { at: at, end: m.index + m[0].length, inner: r[2] ? m[2] : m[1], tag: r[1] };
      }
      if (!best) { mdText(parent, s); return; }
      mdText(parent, s.slice(0, best.at));
      var e = mdEl(best.tag);
      mdEmph(e, best.inner);
      parent.appendChild(e);
      s = s.slice(best.end);
    }
  }
  function mdInline(parent, s) {
    s.split(/(\x60[^\x60\n]+\x60)/).forEach(function (p, i) {
      if (i % 2) { var c = mdEl("code"); c.textContent = p.slice(1, -1); parent.appendChild(c); }
      else if (p) mdEmph(parent, p);
    });
  }
  function mdLinkify(parent, s) {
    var re = new RegExp(MD_LINK.source, "g"), last = 0, m;
    while ((m = re.exec(s))) {
      var url = m[2] || m[3] || m[0], end = m.index + m[0].length;
      if (url === m[0]) {
        var tr = url.match(/[.,;:!?]+$/);
        if (tr) { url = url.slice(0, -tr[0].length); end = m.index + url.length; }
      }
      if (m.index > last) mdInline(parent, s.slice(last, m.index));
      var href = mdSafeURL(url);
      if (href) {
        var a = mdEl("a");
        a.href = href; a.target = "_blank"; a.rel = "noopener noreferrer"; a.title = href;
        a.textContent = m[1] || m[4] || mdLinkLabel(href);
        parent.appendChild(a);
      } else mdText(parent, s.slice(m.index, end));
      last = end;
      re.lastIndex = end;
    }
    if (last < s.length) mdInline(parent, s.slice(last));
  }
  // "1. a 2. b 3. c" or circled numbers crammed into one line go onto separate lines
  function mdBreakLists(t) {
    var cuts = [], re = /(^|[\s。．：:])(\d{1,2})[.)] (?=\S)/g, expected = 1, m;
    while ((m = re.exec(t))) {
      var n = Number(m[2]);
      if (n === expected || (n === 1 && expected > 2)) { cuts.push(m.index + m[1].length); expected = n + 1; }
    }
    var circ = [], cr = /[①-⑳]/g;
    while ((m = cr.exec(t))) circ.push(m.index);
    var all = (cuts.length >= 2 ? cuts : []).concat(circ.length >= 2 ? circ : []);
    if (!all.length) return t;
    all = all.filter(function (x, i) { return all.indexOf(x) === i; }).sort(function (a, b) { return a - b; });
    var out = "", last = 0;
    all.forEach(function (i) {
      if (i === 0) return;
      out += t.slice(last, i).replace(/[ \t]+$/, "") + (t.charAt(i - 1) === "\n" ? "" : "\n");
      last = i;
    });
    return out + t.slice(last);
  }
  function mdRow(row, tag) {
    var tr = mdEl("tr");
    row.trim().replace(/^\||\|$/g, "").split("|").forEach(function (c) { var td = mdEl(tag); mdLinkify(td, c.trim()); tr.appendChild(td); });
    return tr;
  }
  function mdBlocks(parent, text, depth) {
    var lines = text.split("\n"), prose = [], i, j;
    function flush() {
      var s = prose.join("\n").replace(/^\s*\n|\n\s*$/g, "");
      prose = [];
      if (!s.trim()) return;
      var p = mdEl("div", "md-p");
      mdLinkify(p, mdBreakLists(s));
      parent.appendChild(p);
    }
    function list(i) {
      var box = mdEl("div", "md-list"), stack = [], item = null;
      function end() {
        if (!item) return;
        var chk = item.text.match(/^\[([ xX])\]\s+/);
        var li = mdEl("div", "md-li lv" + item.lv + (item.ol ? " ol" : "")), mk = mdEl("span", "mk"), tx = mdEl("span", "lt");
        mk.textContent = chk ? (chk[1] === " " ? "☐" : "☑") : item.mk;
        mk.setAttribute("aria-hidden", "true");
        mdLinkify(tx, mdBreakLists(chk ? item.text.slice(chk[0].length) : item.text));
        li.appendChild(mk); li.appendChild(tx); box.appendChild(li);
        item = null;
      }
      for (; i < lines.length; i++) {
        var l = lines[i], m = l.match(MD_LI);
        if (m) {
          end();
          var ind = m[1].replace(/\t/g, "  ").length;
          while (stack.length && ind < stack[stack.length - 1]) stack.pop();
          if (!stack.length || ind > stack[stack.length - 1]) stack.push(ind);
          var lv = Math.min(stack.length - 1, 4), ol = /\d/.test(m[2]);
          item = { lv: lv, ol: ol, mk: ol ? m[2] : "•◦▪▪▪".charAt(lv), text: m[3], ind: ind };
        } else if (!l.trim()) {
          var next = "";
          for (var k = i + 1; k < lines.length; k++) if (lines[k].trim()) { next = lines[k]; break; }
          if (!next || !MD_LI.test(next)) break;
        } else if (item && /^\s+/.test(l) && l.match(/^\s*/)[0].length > item.ind) item.text += "\n" + l.trim();
        else break;
      }
      end();
      parent.appendChild(box);
      return i;
    }
    for (i = 0; i < lines.length; i++) {
      var l = lines[i], f = l.match(MD_FENCE);
      if (f) {
        j = i + 1;
        while (j < lines.length && lines[j].replace(/^\s+/, "").indexOf(f[1]) !== 0) j++;
        flush();
        var pre = mdEl("pre", "md-code"), code = mdEl("code");
        code.textContent = lines.slice(i + 1, j).join("\n");
        pre.appendChild(code); parent.appendChild(pre);
        i = j;
      } else if (MD_TROW.test(l) && MD_TSEP.test(lines[i + 1] || "")) {
        j = i + 2;
        while (j < lines.length && MD_TROW.test(lines[j])) j++;
        flush();
        var wrap = mdEl("div", "md-tw"), tb = mdEl("table", "md-table"), th = mdEl("thead"), bd = mdEl("tbody");
        th.appendChild(mdRow(l, "th"));
        lines.slice(i + 2, j).forEach(function (r) { bd.appendChild(mdRow(r, "td")); });
        tb.appendChild(th); tb.appendChild(bd); wrap.appendChild(tb); parent.appendChild(wrap);
        i = j - 1;
      } else if (/^#{1,6}\s+\S/.test(l)) {
        flush();
        var h = mdEl("div", "md-h");
        mdLinkify(h, l.replace(/^#{1,6}\s+/, ""));
        parent.appendChild(h);
      } else if (MD_QUOTE.test(l)) {
        j = i;
        while (j < lines.length && MD_QUOTE.test(lines[j])) j++;
        flush();
        var q = mdEl("blockquote", "md-q"), inner = lines.slice(i, j).map(function (x) { return x.replace(MD_QUOTE, ""); }).join("\n");
        if (depth < 4) mdBlocks(q, inner, depth + 1); else mdLinkify(q, inner);
        parent.appendChild(q);
        i = j - 1;
      } else if (MD_RULE.test(l)) {
        flush();
        parent.appendChild(mdEl("hr", "md-hr"));
      } else if (MD_LI.test(l)) {
        flush();
        i = list(i) - 1;
      } else prose.push(l);
    }
    flush();
  }
  // mdRender: the body of one message as a <div class="text md">
  function mdRender(text) {
    var box = mdEl("div", "text md");
    mdBlocks(box, String(text == null ? "" : text).replace(/\r\n?/g, "\n"), 0);
    return box;
  }
`

// viewObsJS: search, health panel, message links, background-tab title, keys. Uses the
// closure of app.js (S, E, RP, el, render, selectPane, openThread, loadOlder, ...).
const viewObsJS = `
  // --- search and filters ---
  // Words match the text (and file name), all of them; from:/to: a name (an unknown name
  // matches as a prefix); in:/channel: or #channel; in:dm; is:unread|read|undelivered|thread.
  // The same key given twice is either one; different keys combine.
  var Q = { raw: "", q: null, timer: 0 }, H = { on: false }, NOTICE = { n: 0 }, SEARCH_MAX = 300;
  function parseQuery(s) {
    var q = { from: [], to: [], ch: [], is: [], words: [] };
    String(s).split(/\s+/).forEach(function (w) {
      if (!w) return;
      var m = w.match(/^(from|to|in|channel|is):(.+)$/i);
      if (m) { var k = m[1].toLowerCase(); (k === "in" || k === "channel" ? q.ch : q[k]).push(m[2].toLowerCase()); }
      else if (w.charAt(0) === "#" && S.channels.indexOf(w.slice(1).toLowerCase()) >= 0) q.ch.push(w.slice(1).toLowerCase());
      else q.words.push(w.toLowerCase());
    });
    return q;
  }
  function knownName(v) { return v === "all" || v === VIEWER || S.members.indexOf(v) >= 0; }
  function nameIs(n, vs) {
    n = String(n).toLowerCase();
    return vs.some(function (v) { return n === v || (!knownName(v) && n.indexOf(v) === 0); });
  }
  function isState(m, v) {
    if (v === "unread") return m.from !== VIEWER && m.id > readMark(paneOf(m));
    if (v === "read") return m.from !== VIEWER && m.id <= readMark(paneOf(m));
    if (v === "undelivered") return (S.un[m.id] || 0) > 0;
    if (v === "thread") return !!m.reply_to;
    return false;
  }
  function matchQuery(m, q) {
    if (q.from.length && !nameIs(m.from, q.from)) return false;
    if (q.to.length && !nameIs(m.to, q.to)) return false;
    if (q.ch.length && !q.ch.some(function (c) { return c === "dm" ? m.to !== "all" : String(m.channel).toLowerCase() === c; })) return false;
    if (q.is.length && !q.is.every(function (v) { return isState(m, v); })) return false;
    if (q.words.length) {
      if (m._lc === undefined) m._lc = (String(m.text || "") + " " + (m.file ? m.file.name : m.file_id || "")).toLowerCase();
      if (!q.words.every(function (w) { return m._lc.indexOf(w) >= 0; })) return false;
    }
    return true;
  }
  function setQuery(s) {
    s = String(s || "");
    if (E.q.value !== s) E.q.value = s;
    Q.raw = s.trim();
    Q.q = Q.raw ? parseQuery(Q.raw) : null;
    E.qClear.hidden = !Q.raw;
    if (Q.q && H.on) setHealth(false, true);
    if (Q.q) document.body.setAttribute("data-pane", "chat");
    S.flashAfter = Infinity;
    render(true);
  }
  function renderSearch(list, ix, stick) {
    var hits = list.filter(function (m) { return matchQuery(m, Q.q); }), shown = hits.slice(-SEARCH_MAX);
    var info = hits.length ? hits.length + (hits.length === 1 ? " match" : " matches") + " in all channels and DMs" +
      (hits.length > shown.length ? ", newest " + shown.length + " shown" : "") : "No match";
    if (S.hasOlder) info += " (loaded messages only)";
    var items = [el("li", "qinfo", info)].concat(buildList(shown, ix.replies, false, true, undefined, true));
    E.list.replaceChildren.apply(E.list, items);
    E.older.hidden = !S.hasOlder || RP.on;
    if (stick) E.scroll.scrollTop = E.scroll.scrollHeight;
    E.newbtn.hidden = true;
  }

  // --- health panel ---
  // Counted in the page from the loaded messages and the delivery receipts the view already
  // has: sent, received, still unfetched, median time to fetch, last message, quiet members
  // and who sends to whom, over the chosen range.
  function fmtDur(ms) {
    var m = Math.round(ms / 60000);
    if (m < 1) return Math.max(0, Math.round(ms / 1000)) + "s";
    if (m < 60) return m + "m";
    var h = m / 60;
    if (h < 48) return (h < 10 ? h.toFixed(1).replace(/\.0$/, "") : String(Math.round(h))) + "h";
    return Math.round(h / 24) + "d";
  }
  function median(xs) { if (!xs.length) return null; var v = xs.slice().sort(function (a, b) { return a - b; }); return v[Math.floor((v.length - 1) / 2)]; }
  function healthStats(list) {
    var rng = E.hRange.value, now = Date.now(), from = rng === "all" ? -Infinity : now - Number(rng) * 86400000;
    var names = [], st = {}, mx = {}, out = { total: 0, first: 0, hasAll: false };
    function row(n) {
      if (!st[n]) { st[n] = { s: 0, r: 0, un: 0, times: [], last: 0 }; names.push(n); }
      return st[n];
    }
    S.members.forEach(row);
    list.forEach(function (m) {
      var t = tsMs(m), a = row(m.from);
      if (t > a.last) a.last = t;
      if (t < from) return;
      if (!out.first) out.first = t;
      out.total++; a.s++;
      var rcs = S.rc[m.id], tos;
      if (rcs && rcs.length) tos = rcs.map(function (r) { return r.name; });
      else if (m.to !== "all") tos = [m.to];
      else tos = S.members.filter(function (n) { return n !== m.from && (S.memberCh[n] || []).indexOf(m.channel) >= 0; });
      tos.forEach(function (n) { if (n !== VIEWER) row(n).r++; });
      var key = m.from + ">" + m.to;
      mx[key] = (mx[key] || 0) + 1;
      if (m.to === "all") out.hasAll = true;
      if (rcs) rcs.forEach(function (r) {
        var b = row(r.name);
        if (r.status === "pending") b.un++;
        else if (r.status === "delivered" && r.delivered) { var d = Date.parse(r.delivered) - t; if (d >= 0) b.times.push(d); }
      });
    });
    out.names = names; out.st = st; out.mx = mx; out.from = from;
    return out;
  }
  function hCell(tr, tag, text, cls) { var c = el(tag, cls || "", text); tr.appendChild(c); return c; }
  function hName(n, withDot) {
    var w = el("span", "mem " + colorClass(n));
    w.appendChild(avatar(n, true));
    w.appendChild(el("span", "mname", n));
    if (withDot && S.alive && S.alive[n] !== undefined) {
      var d = el("span", "ldot " + (S.alive[n] ? "on" : "off"));
      d.title = S.alive[n] ? "alive" : "no contact for 10 min";
      w.appendChild(d);
    }
    return w;
  }
  function renderHealth(list) {
    var hs = healthStats(list || visible()), now = Date.now(), qh = Number(E.hQuiet.value) || 24, quiet = [];
    var thead = el("thead"), hr = el("tr"), tbody = el("tbody");
    [["Member", ""], ["Sent", "num"], ["Received", "num"], ["Unread", "num"], ["Median to read", ""], ["Last sent", ""]].forEach(function (h) { hCell(hr, "th", h[0], h[1]); });
    hr.children[3].title = "recipients that have not fetched the message yet (delivery receipts)";
    hr.children[4].title = "median time from sending until the recipient fetched it (delivery receipts; fetched is not proof that it was read). In brackets: how many";
    thead.appendChild(hr);
    hs.names.forEach(function (n) {
      var a = hs.st[n], tr = el("tr", "click"), td = el("td"), w = hName(n, true), me = n === VIEWER;
      if (!me && S.members.indexOf(n) >= 0 && (!a.last || now - a.last >= qh * 3600000)) {
        quiet.push(n + " (" + (a.last ? fmtDur(now - a.last) : "none loaded") + ")");
        var q = el("span", "warn", "⚠");
        q.title = a.last ? "quiet for " + fmtDur(now - a.last) : "no message from this member in the loaded history";
        w.appendChild(q);
      }
      td.appendChild(w); tr.appendChild(td);
      hCell(tr, "td", String(a.s), "num" + (a.s ? "" : " faint"));
      hCell(tr, "td", me ? "–" : String(a.r), "num" + (a.r && !me ? "" : " faint"));
      hCell(tr, "td", me ? "–" : String(a.un), "num" + (a.un && !me ? " warn" : " faint"));
      var mid = median(a.times);
      hCell(tr, "td", mid === null ? "–" : fmtDur(mid) + " (" + a.times.length + ")", mid === null ? "faint" : "");
      var ls = hCell(tr, "td", a.last ? jstString(a.last).slice(5, 16) + " · " + fmtDur(now - a.last) + " ago" : "–", a.last ? "" : "faint");
      if (a.last) ls.title = jstString(a.last) + " JST";
      tr.title = "search messages from " + n;
      tr.addEventListener("click", function () { setQuery("from:" + n); });
      tbody.appendChild(tr);
    });
    E.hMembers.replaceChildren(thead, tbody);
    E.hQuietList.textContent = quiet.length ? "⚠ Quiet longer than " + fmtDur(qh * 3600000) + ": " + quiet.join(", ") :
      "Nobody quiet longer than " + fmtDur(qh * 3600000) + ".";
    E.hQuietList.className = "hquiet" + (quiet.length ? " warn" : "");
    // who sends to whom: rows send, columns receive ("all" = channel messages)
    var act = hs.names.filter(function (n) { return hs.st[n].s || hs.st[n].r; });
    var cols = act.filter(function (n) { return n !== VIEWER; }).concat(hs.hasAll ? ["all"] : []), cmax = 1;
    Object.keys(hs.mx).forEach(function (k) { if (hs.mx[k] > cmax) cmax = hs.mx[k]; });
    var mh = el("thead"), mr = el("tr"), mb = el("tbody");
    mr.appendChild(el("th"));
    cols.forEach(function (c) {
      var th = el("th", "col");
      th.title = c === "all" ? "to all (channel messages)" : c;
      th.appendChild(el("span", c === "all" ? "" : "mname " + colorClass(c), c));
      mr.appendChild(th);
    });
    mh.appendChild(mr);
    act.filter(function (n) { return hs.st[n].s; }).forEach(function (from) {
      var tr = el("tr"), th = el("th", "row");
      th.appendChild(el("span", "mname " + colorClass(from), from));
      tr.appendChild(th);
      cols.forEach(function (to) {
        if (from === to) { tr.appendChild(el("td", "cell zero self")); return; }
        var n = hs.mx[from + ">" + to] || 0;
        if (!n) { tr.appendChild(el("td", "cell zero", "·")); return; }
        var td = el("td", "cell", String(n));
        td.style.setProperty("--p", Math.round(10 + 50 * Math.sqrt(n / cmax)) + "%");
        td.title = from + " → " + to + ": " + n + " (click to search them)";
        td.addEventListener("click", function () { setQuery("from:" + from + " to:" + to); });
        tr.appendChild(td);
      });
      mb.appendChild(tr);
    });
    E.hMatrix.replaceChildren(mh, mb);
    var sum = hs.total + (hs.total === 1 ? " message" : " messages") + " in range";
    if (S.hasOlder && (hs.from === -Infinity || !S.all.length || tsMs(S.all[0]) > hs.from)) sum += " · loaded from " + (S.all.length ? jstString(tsMs(S.all[0])).slice(0, 16) : "-") + " only";
    E.hSum.textContent = sum;
    E.hOlder.hidden = !S.hasOlder;
  }
  function setHealth(on, quiet) {
    H.on = on;
    E.health.hidden = !on;
    E.scroll.hidden = on;
    E.hBtn.classList.toggle("on", on);
    E.hBtn.setAttribute("aria-pressed", on ? "true" : "false");
    if (on) { E.newbtn.hidden = true; document.body.setAttribute("data-pane", "chat"); }
    if (!quiet) { S.flashAfter = Infinity; render(true); }
  }

  // --- links to one message: /view#m=<id> ---
  function linkOf(id) { return window.location.origin + "/view#m=" + id; }
  function toast(text, ms) {
    E.toast.textContent = text;
    E.toast.classList.add("on");
    clearTimeout(E.toast._t);
    E.toast._t = setTimeout(function () { E.toast.classList.remove("on"); }, ms || 1600);
  }
  function copyFallback(text) {
    var ta = el("textarea", "offscreen");
    ta.value = text; ta.setAttribute("readonly", "");
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) {}
    ta.remove();
    return ok;
  }
  function copyText(text, what) {
    var done = function (ok) { toast(ok ? what + " copied" : "Copy failed: " + text, ok ? 1600 : 6000); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(copyFallback(text)); });
    } else done(copyFallback(text));
  }
  function copyLink(id) {
    try { history.replaceState(null, "", "#m=" + id); } catch (e) {}
    copyText(linkOf(id), "Link to #" + id);
  }
  function openLinked() {
    var mm = /^#m=(\d+)$/.exec(window.location.hash);
    if (mm && S.loaded) reveal(Number(mm[1]), 0);
  }
  // reveal: go to the message's channel or DM (its thread when it is a reply there), loading
  // older history first when needed, then scroll to it and highlight it
  function reveal(id, tries) {
    var m = S.byId.get(id);
    if (!m) {
      if (S.hasOlder && S.all.length && id < S.all[0].id && tries < 9) {
        E.status.textContent = "loading older messages for #" + id;
        loadOlder().then(function () { reveal(id, tries + 1); });
        return;
      }
      toast("Message #" + id + (id > S.last ? " does not exist yet" : " is not loaded"), 4000);
      return;
    }
    if (RP.on) rpExit();
    if (Q.raw) { Q.raw = ""; Q.q = null; E.q.value = ""; E.qClear.hidden = true; }
    if (H.on) setHealth(false, true);
    var root = m.reply_to ? S.byId.get(m.reply_to) : null, inTh = !!root && paneOf(root) === paneOf(m);
    S.hl = id; S.sel = id;
    selectPane(inTh ? paneOf(root) : paneOf(m));
    if (inTh) openThread(root.id);
    var li = (inTh ? E.thList : E.list).querySelector('li.msg[data-id="' + id + '"]');
    if (li) li.scrollIntoView({ block: "center" });
    clearTimeout(S.hlTimer);
    S.hlTimer = setTimeout(function () {
      S.hl = 0;
      Array.prototype.forEach.call(document.querySelectorAll("li.msg.hl"), function (x) { x.classList.remove("hl"); });
    }, 4000);
  }

  // --- background tab: the title counts messages that came while it was hidden ---
  function noticeNew(got) {
    if (!document.hidden) return;
    NOTICE.n += got.filter(function (m) { return m.from !== VIEWER; }).length;
    setTitle();
  }
  function setTitle(total) {
    if (total !== undefined) S.total = total;
    document.title = (document.hidden && NOTICE.n ? "(" + NOTICE.n + " new) " : S.total && !RP.on ? "(" + S.total + ") " : "") + "AgentsAgora (A2)";
  }

  // --- keyboard; never while typing in a field ---
  function typing(t) { return !!t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || "")); }
  function navItems() {
    var box = E.scroll.offsetParent === null && S.thread ? E.thList : E.list;
    return Array.prototype.slice.call(box.querySelectorAll("li.msg[data-id]"));
  }
  function selectLi(li) {
    Array.prototype.forEach.call(document.querySelectorAll("li.msg.sel"), function (x) { x.classList.remove("sel"); });
    S.sel = Number(li.getAttribute("data-id"));
    li.classList.add("sel");
    li.scrollIntoView({ block: "nearest" });
  }
  function step(dir) {
    if (H.on) return;
    var items = navItems();
    if (!items.length) return;
    var i = -1;
    for (var k = 0; k < items.length; k++) if (Number(items[k].getAttribute("data-id")) === S.sel) { i = k; break; }
    if (i < 0) {
      // nothing selected yet: start from the last message fully on screen
      var bottom = items[0].parentNode.parentNode.getBoundingClientRect().bottom;
      i = items.length - 1;
      for (k = 0; k < items.length; k++) if (items[k].getBoundingClientRect().bottom > bottom + 1) { i = Math.max(0, k - 1); break; }
    } else i = Math.max(0, Math.min(items.length - 1, i + dir));
    selectLi(items[i]);
  }
  function edge(last) { var items = navItems(); if (items.length && !H.on) selectLi(items[last ? items.length - 1 : 0]); }
  function selMsg() {
    var m = S.sel ? S.byId.get(S.sel) : null;
    if (!m) toast("Select a message first (j / k)");
    return m;
  }
  function openSel(focus) {
    var m = selMsg();
    if (!m || RP.on) return;
    if (Q.q) { reveal(m.id, 0); return; }
    openThread(m.reply_to || m.id);
    if (focus && !C.thread.text.disabled) C.thread.text.focus();
  }
  function toggleHelp(open) { E.help.hidden = !open; if (open) $("help-close").focus(); }
  var KEYS = {
    "/": function () { if (H.on) setHealth(false); document.body.setAttribute("data-pane", "chat"); E.q.focus(); E.q.select(); },
    "?": function () { toggleHelp(true); },
    j: function () { step(1); }, k: function () { step(-1); },
    g: function () { edge(false); }, G: function () { edge(true); },
    Enter: function () { openSel(false); }, r: function () { openSel(true); },
    l: function () { var m = selMsg(); if (m) copyLink(m.id); },
    y: function () { var m = selMsg(); if (m) copyText(m.text || "", "Text of #" + m.id); },
    h: function () { setHealth(!H.on); }
  };
  function onKey(ev) {
    if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.isComposing || ev.keyCode === 229) return;
    if (!E.help.hidden) {
      if (ev.key === "Escape" || ev.key === "?") { ev.preventDefault(); toggleHelp(false); }
      return;
    }
    var t = ev.target;
    if (typing(t)) {
      if (t === E.q && ev.key === "Escape") { ev.preventDefault(); setQuery(""); E.q.blur(); }
      else if (t === E.q && ev.key === "Enter") { ev.preventDefault(); clearTimeout(Q.timer); setQuery(E.q.value); E.q.blur(); }
      else if (ev.key === "Escape" && S.thread) closeThread(false);
      return;
    }
    if (ev.key === "Escape") {
      if (Q.raw) setQuery(""); else if (H.on) setHealth(false); else if (S.thread) closeThread(false);
      else { S.sel = 0; Array.prototype.forEach.call(document.querySelectorAll("li.msg.sel"), function (x) { x.classList.remove("sel"); }); }
      return;
    }
    if ((ev.key === "Enter" || ev.key === " ") && t && t.closest && t.closest("button,a,summary,details")) return;
    var act = KEYS[ev.key];
    if (act) { ev.preventDefault(); act(); }
  }

  function obsInit() {
    E.q = $("q"); E.qClear = $("q-clear"); E.health = $("health"); E.hBtn = $("h-btn"); E.hRange = $("h-range");
    E.hQuiet = $("h-quiet"); E.hSum = $("h-sum"); E.hMembers = $("h-members"); E.hMatrix = $("h-matrix");
    E.hQuietList = $("h-quietlist"); E.hOlder = $("h-older"); E.help = $("help"); E.toast = $("toast");
    try {
      var hr = localStorage.getItem("a2-view-hrange"), hq = localStorage.getItem("a2-view-quiet");
      if (hr && E.hRange.querySelector('option[value="' + hr.replace(/[^a-z0-9]/g, "") + '"]')) E.hRange.value = hr;
      if (hq && E.hQuiet.querySelector('option[value="' + hq.replace(/[^0-9]/g, "") + '"]')) E.hQuiet.value = hq;
    } catch (e) {}
    E.q.addEventListener("input", function () {
      clearTimeout(Q.timer);
      Q.timer = setTimeout(function () { setQuery(E.q.value); }, 200);
    });
    E.qClear.addEventListener("click", function () { setQuery(""); E.q.focus(); });
    E.hBtn.addEventListener("click", function () { setHealth(!H.on); });
    [[E.hRange, "a2-view-hrange"], [E.hQuiet, "a2-view-quiet"]].forEach(function (p) {
      p[0].addEventListener("change", function () { try { localStorage.setItem(p[1], p[0].value); } catch (e) {} renderHealth(); });
    });
    E.hOlder.addEventListener("click", function () { E.hOlder.disabled = true; loadOlder().then(function () { E.hOlder.disabled = false; }); });
    $("keys-btn").addEventListener("click", function () { toggleHelp(true); });
    $("help-close").addEventListener("click", function () { toggleHelp(false); });
    E.help.addEventListener("click", function (ev) { if (ev.target === E.help) toggleHelp(false); });
    document.addEventListener("keydown", onKey);
    window.addEventListener("hashchange", openLinked);
  }
`

// viewObsCSS is appended to viewCSS.
const viewObsCSS = `.md .md-p{white-space:pre-wrap}
.md code{font:12.5px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:var(--chip);border-radius:4px;padding:0 4px}
.md-code{white-space:pre;overflow-x:auto;background:var(--chip);border-radius:6px;padding:8px 10px;margin:4px 0;font:12.5px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.md-code code{background:none;padding:0}
.md-tw{overflow-x:auto;max-width:100%;margin:4px 0}
.md-table{border-collapse:collapse;font-size:13px}
.md-table th,.md-table td{border:1px solid var(--line);padding:3px 8px;text-align:left;vertical-align:top;white-space:pre-wrap}
.md-table th{background:var(--side)}
.md-h{font-weight:700;margin:6px 0 2px}
.md-q{margin:4px 0;padding:0 0 0 10px;border-left:3px solid var(--line);color:var(--muted)}
.md-hr{border:0;border-top:1px solid var(--line);margin:8px 0}
.md-list{margin:2px 0}
.md-li{display:flex;gap:6px}
.md-li .mk{flex:none;min-width:1em;color:var(--muted);text-align:right}
.md-li.ol .mk{min-width:1.6em;font-variant-numeric:tabular-nums}
.md-li .lt{flex:1;min-width:0;white-space:pre-wrap}
.md-li.lv1{padding-left:18px}.md-li.lv2{padding-left:36px}.md-li.lv3{padding-left:54px}.md-li.lv4{padding-left:72px}
.md a{color:var(--accent);text-decoration:underline;overflow-wrap:anywhere}
a.mid{text-decoration:none}a.mid:hover{text-decoration:underline}
.qbar{display:flex;align-items:center;gap:4px;padding:6px 14px;border-bottom:1px solid var(--line)}
.qbar input{flex:1;min-width:0;border:1px solid var(--line);border-radius:6px;background:var(--input);padding:4px 8px}
.qbar input::-webkit-search-cancel-button{display:none}
.qinfo{color:var(--muted);font-size:12px;padding:4px 16px 6px}
#h-btn.on{background:var(--on);color:var(--on-fg);border-color:var(--on)}
.msg.sel{box-shadow:inset 3px 0 0 var(--accent)}
.msg.hl{animation:hl 4s ease-out}
@keyframes hl{0%,50%{background:var(--fresh)}100%{background:transparent}}
.health{flex:1;overflow:auto;padding:10px 14px 16px;overscroll-behavior:contain}
.hbar{display:flex;flex-wrap:wrap;gap:6px 14px;align-items:center;font-size:13px;color:var(--muted);margin-bottom:10px}
.hcard{border:1px solid var(--line);border-radius:8px;padding:8px 10px;margin-bottom:12px}
.hcard h3{font-size:14px;margin:0 0 6px}
.hint{font-size:12px;color:var(--muted);margin:0 0 6px}
.htw{overflow-x:auto}
.ht{border-collapse:collapse;font-size:13px;font-variant-numeric:tabular-nums}
.ht th,.ht td{padding:3px 8px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}
.ht th{font-weight:600;color:var(--muted)}
.ht .num{text-align:right}
.ht tr.click{cursor:pointer}.ht tr.click:hover{background:var(--hover)}
.faint{color:var(--muted)}.warn{color:var(--badge)}
.hquiet{font-size:13px;margin:8px 0 0}
.mx td.cell{text-align:center;min-width:30px;cursor:pointer;background:color-mix(in srgb,var(--accent) var(--p,0%),transparent)}
.mx td.zero{color:var(--muted);cursor:default;background:transparent}
.mx td.self{background:var(--hover)}
.mx th.col{vertical-align:bottom;padding:4px 2px;text-align:center}
.mx th.col span{writing-mode:vertical-rl;transform:rotate(180deg);display:inline-block;max-height:140px;overflow:hidden;text-overflow:ellipsis}
.mx th.row{text-align:right}
.help{position:fixed;inset:0;background:rgba(0,0,0,.45);display:flex;align-items:center;justify-content:center;z-index:10}
.helpbox{background:var(--bg);border-radius:10px;padding:16px 20px;max-width:460px;width:calc(100% - 32px);max-height:85vh;overflow:auto;box-shadow:var(--shadow)}
.helpbox h3{margin:0 0 8px;font-size:16px}
.keys{display:grid;grid-template-columns:auto 1fr;gap:4px 14px;margin:8px 0;font-size:14px}
.keys dt{font-family:ui-monospace,Menlo,Consolas,monospace;font-weight:700;white-space:nowrap}
.keys dd{margin:0}
.toast{position:fixed;left:50%;bottom:24px;transform:translateX(-50%);background:var(--fg);color:var(--bg);padding:6px 14px;border-radius:16px;font-size:13px;opacity:0;pointer-events:none;transition:opacity .2s;z-index:11;max-width:90vw;overflow-wrap:anywhere}
.toast.on{opacity:1}
.offscreen{position:fixed;left:-9999px;top:0;opacity:0}
@media (hover:none){#keys-btn{display:none}}
@media (max-width:760px){.qbar{padding:4px 10px}.health{padding:8px 10px}#head{gap:6px;padding:6px 10px}#head button.ghost{padding:4px 8px}}
`
