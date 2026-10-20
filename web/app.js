/* golang-sandbox frontend logic. No frameworks, no CDN, CSP-friendly.
   All untrusted output (program stdout/stderr, compile errors) is rendered
   via textContent only — never innerHTML — to rule out stored XSS. */
"use strict";

(function () {
  // ---------- dom ----------
  const $ = (id) => document.getElementById(id);
  const codeEl = $("code"), highlightEl = $("highlight"), highlightCode = $("highlight-code");
  const gutterEl = $("gutter"), errLayerEl = $("errlayer"), stdinEl = $("stdin"), stdinWrap = $("stdin-wrap");
  const btnRun = $("btn-run"), btnStop = $("btn-stop"), btnFormat = $("btn-format"), btnStdin = $("btn-stdin-toggle");
  const btnClear = $("btn-clear"), btnCopy = $("btn-copy"), btnReset = $("btn-reset"), saveState = $("save-state");
  const statusChip = $("status-chip"), durationChip = $("duration-chip"), traceChip = $("trace-chip");
  const noticeEl = $("notice"), queueInfo = $("queue-info"), placeholderEl = $("placeholder"), worklineEl = $("workline");
  const stdoutSection = $("stdout-section"), stderrSection = $("stderr-section");
  const stdoutEl = $("stdout"), stderrEl = $("stderr");
  const lineColEl = $("line-col"), codeSizeEl = $("code-size");
  const restoreBar = $("restore-bar"), restoreYes = $("restore-yes"), restoreNo = $("restore-no"), restoreWhen = $("restore-when");
  const queueChip = $("queue-chip"), capEl = $("capacity"), goversionChip = $("goversion-chip"), limitsNote = $("limits-note");
  const infoBtn = $("info-btn"), infoPop = $("info-pop"), toastsEl = $("toasts");

  const DEFAULT_TEMPLATE =
    "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, 世界\")\n\tfmt.Println(\"Go sandbox ready.\")\n}\n";

  const DRAFT_KEY = "gsb.draft", DRAFT_AT_KEY = "gsb.draft.at", STDIN_KEY = "gsb.stdin";
  const state = { runId: null, pollTimer: null, busy: false, mode: "idle", lastTrace: "", queueStart: 0, runStart: 0, outText: "" };

  // ---------- go highlighter ----------
  const KEYWORDS = new Set(("break case chan const continue default defer else fallthrough for func go goto " +
    "if import interface map package range return select struct switch type var").split(" "));
  const BUILTINS = new Set(("append cap close complex copy delete imag len make new panic print println real recover " +
    "true false iota nil string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr " +
    "float32 float64 complex64 complex128 byte rune bool error any").split(" "));

  function escapeHtml(s) {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  function span(cls, text) {
    return cls ? '<span class="' + cls + '">' + escapeHtml(text) + "</span>" : escapeHtml(text);
  }

  function isIdentStart(c) { return /[A-Za-z_]/.test(c); }
  function isIdent(c) { return /[A-Za-z0-9_]/.test(c); }

  function highlightGo(src) {
    let out = "", i = 0;
    const n = src.length;
    while (i < n) {
      const c = src[i], rest = src.slice(i);
      if (rest.startsWith("//")) {
        let j = src.indexOf("\n", i); if (j < 0) j = n;
        out += span("tk-com", src.slice(i, j)); i = j; continue;
      }
      if (rest.startsWith("/*")) {
        let j = src.indexOf("*/", i + 2); j = j < 0 ? n : j + 2;
        out += span("tk-com", src.slice(i, j)); i = j; continue;
      }
      if (c === '"' || c === "'" || c === "`") {
        let j = i + 1;
        while (j < n) {
          if (src[j] === "\\" && c !== "`") j += 2;
          else if (src[j] === c) { j++; break; }
          else if (c !== "`" && src[j] === "\n") break; // unterminated: stop at EOL
          else j++;
        }
        out += span("tk-str", src.slice(i, j)); i = j; continue;
      }
      if (/[0-9]/.test(c) || (c === "." && /[0-9]/.test(src[i + 1] || ""))) {
        let j = i + 1;
        while (j < n && /[0-9a-zA-Z_.]/.test(src[j])) j++;
        if (/[eEpP]$/.test(src.slice(i, j)) && (src[j] === "+" || src[j] === "-")) j++;
        out += span("tk-num", src.slice(i, j)); i = j; continue;
      }
      if (isIdentStart(c)) {
        let j = i + 1;
        while (j < n && isIdent(src[j])) j++;
        const word = src.slice(i, j);
        let k = j;
        while (k < n && src[k] === " ") k++;
        if (KEYWORDS.has(word)) out += span("tk-kw", word);
        else if (BUILTINS.has(word)) out += span("tk-builtin", word);
        else if (src[k] === "(") out += span("tk-fn", word);
        else out += escapeHtml(word);
        i = j; continue;
      }
      out += escapeHtml(c); i++;
    }
    return out;
  }

  // ---------- status & banner rendering helpers ----------
  // Static, trusted inline SVGs (aria-hidden). Never fed with user data.
  const ICONS = {
    queue: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 6h11M10 12h11M10 18h11"/><path d="M4 6h1v4M4 10h2M6 18H4c0-1 2-2 2-3s-1-1.5-2-1"/></svg>',
    check: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="m8.5 12.2 2.6 2.6 4.9-5.4"/></svg>',
    octagon: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M7.86 2h8.28L22 7.86v8.28L16.14 22H7.86L2 16.14V7.86L7.86 2z"/><path d="m15 9-6 6M9 9l6 6"/></svg>',
    alert: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/></svg>',
    clock: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>',
    ban: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="m4.93 4.93 14.14 14.14"/></svg>',
    ram: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 7a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1z"/><path d="M6 17v-2M10 17v-2M14 17v-2M18 17v-2M8 9V7M12 9V7M16 9V7"/></svg>',
    info: '<svg class="ic" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg>',
    note: '<svg class="ic" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg>',
    warn: '<svg class="ic" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/></svg>',
  };
  const SPIN_HTML = '<span class="spin" aria-hidden="true"></span>';

  function toast(kind, title, body) {
    const box = document.createElement("div");
    box.className = "toast t-" + kind;
    const ic = document.createElement("span");
    ic.className = "t-ic";
    ic.innerHTML = kind === "rose" ? ICONS.octagon : kind === "ok" ? ICONS.check : ICONS.info;
    const main = document.createElement("div");
    const tt = document.createElement("div"); tt.className = "t-tt"; tt.textContent = title;
    const bd = document.createElement("div"); bd.className = "t-bd"; bd.textContent = body || "";
    main.append(tt, bd);
    box.append(ic, main);
    toastsEl.appendChild(box);
    setTimeout(() => { box.classList.add("out"); setTimeout(() => box.remove(), 300); }, 3600);
  }

  // Banner in the output canvas: icon + title + body (+errors/note/progress/action),
  // colored per the spec §3.1 status families (bright-on-tint on the dark canvas).
  function renderBanner(container, opt) {
    container.textContent = "";
    if (!opt) { container.hidden = true; return; }
    const w = document.createElement("div");
    w.className = "banner b-" + (opt.kind || "neutral");
    const ic = document.createElement("div");
    ic.className = "bn-ic";
    ic.innerHTML = ICONS[opt.icon] || ICONS.info;
    const main = document.createElement("div");
    main.className = "bn-main";
    const tt = document.createElement("div"); tt.className = "bn-tt"; tt.textContent = opt.title; main.appendChild(tt);
    if (opt.body) { const bd = document.createElement("div"); bd.className = "bn-bd"; bd.textContent = opt.body; main.appendChild(bd); }
    for (const e of opt.errors || []) main.appendChild(errRow(e));
    if (opt.progress !== undefined) {
      const tr = document.createElement("progress");
      tr.className = "q-track"; tr.max = 100; tr.value = opt.progress;
      main.appendChild(tr);
      const lf = document.createElement("div"); lf.className = "q-left"; lf.id = "q-left";
      lf.textContent = opt.progressLabel || ""; main.appendChild(lf);
    }
    if (opt.note) {
      const nd = document.createElement("div"); nd.className = "bn-note";
      nd.innerHTML = ICONS.note;
      nd.appendChild(document.createTextNode(opt.note));
      main.appendChild(nd);
    }
    if (opt.action) {
      const box = document.createElement("div"); box.className = "bn-act";
      const bt = document.createElement("button"); bt.type = "button"; bt.className = "bbtn";
      bt.textContent = opt.action.label;
      bt.dataset.act = opt.action.act;
      if (opt.action.cooldown) bt.dataset.cooldown = "1";
      box.appendChild(bt); main.appendChild(box);
    }
    w.append(ic, main);
    container.appendChild(w);
    container.hidden = false;
  }

  function errRow(e) {
    const r = document.createElement("button");
    r.type = "button";
    r.className = "err-row";
    const pos = document.createElement("span"); pos.className = "err-pos";
    pos.textContent = e.line ? "main.go:" + e.line + (e.col ? ":" + e.col : "") : "错误";
    const msg = document.createElement("span"); msg.className = "err-msg";
    msg.textContent = e.message || e.text || "";
    r.append(pos, msg);
    if (e.line > 0) r.addEventListener("click", () => gotoLine(e.line));
    else r.disabled = true;
    return r;
  }

  // ---------- compile-error line backdrop ----------
  let errLines = new Set();

  function buildErrLayer() {
    const lines = codeEl.value.split("\n").length;
    if (errLayerEl.childElementCount !== lines) {
      errLayerEl.textContent = "";
      const frag = document.createDocumentFragment();
      for (let i = 0; i < lines; i++) frag.appendChild(document.createElement("div"));
      errLayerEl.appendChild(frag);
    }
    for (let i = 0; i < errLayerEl.children.length; i++) {
      errLayerEl.children[i].classList.toggle("line-err", errLines.has(i + 1));
    }
  }

  function clearErrMarks() {
    errLines = new Set();
    buildErrLayer();
  }

  // ---------- editor plumbing ----------
  function refreshEditor() {
    const code = codeEl.value;
    highlightCode.innerHTML = highlightGo(code) + "\n";
    const lines = code.split("\n").length;
    let g = "";
    for (let i = 1; i <= lines; i++) g += "<span>" + i + "</span>";
    gutterEl.innerHTML = g;
    buildErrLayer();
    updateCursorInfo();
  }

  function updateCursorInfo() {
    const pos = codeEl.selectionStart;
    const upto = codeEl.value.slice(0, pos);
    const line = upto.split("\n").length;
    const col = pos - upto.lastIndexOf("\n");
    lineColEl.textContent = "Ln " + line + ", Col " + col;

    const bytes = new TextEncoder().encode(codeEl.value).length;
    const max = META.code_max_kb || 64;
    const kb = bytes / 1024;
    codeSizeEl.textContent = (kb >= 1 ? kb.toFixed(1) + " KB" : bytes + " B") + " / " + max + " KB";
    codeSizeEl.classList.toggle("over", bytes > max * 1024);
    return line;
  }

  function syncScroll() {
    highlightEl.scrollTop = codeEl.scrollTop;
    highlightEl.scrollLeft = codeEl.scrollLeft;
    gutterEl.scrollTop = codeEl.scrollTop;
    errLayerEl.scrollTop = codeEl.scrollTop;
  }

  function setSelection(start, end) {
    codeEl.focus();
    codeEl.setSelectionRange(start, end);
    updateCursorInfo();
  }

  function currentLineIndent(value, pos) {
    const lineStart = value.lastIndexOf("\n", pos - 1) + 1;
    const m = value.slice(lineStart, pos).match(/^[ \t]*/);
    return m ? m[0] : "";
  }

  function replaceRange(start, end, text, caret) {
    codeEl.setRangeText(text, start, end, "end");
    if (caret !== undefined) codeEl.setSelectionRange(caret, caret);
    refreshEditor(); saveDraft();
  }

  codeEl.addEventListener("input", refreshEditor);
  codeEl.addEventListener("scroll", syncScroll);

  codeEl.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); submitRun(); return; }

    if (e.key === "Tab") {
      e.preventDefault();
      const s = codeEl.selectionStart, t = codeEl.selectionEnd;
      if (e.shiftKey) {
        const ls = codeEl.value.lastIndexOf("\n", s - 1) + 1;
        if (codeEl.value[ls] === "\t") replaceRange(ls, ls + 1, "", Math.max(ls, s - 1));
      } else if (s !== t && codeEl.value.slice(s, t).includes("\n")) {
        // indent each selected line
        const v = codeEl.value, ls = v.lastIndexOf("\n", s - 1) + 1;
        const block = v.slice(ls, t).replace(/^/gm, "\t");
        replaceRange(ls, t, block, s + 1);
      } else {
        replaceRange(s, t, "\t");
      }
      return;
    }

    if (e.key === "Enter") {
      const s = codeEl.selectionStart;
      if (s !== codeEl.selectionEnd) return;
      const before = codeEl.value[s - 1] || "", afterRaw = codeEl.value.slice(s);
      const indent = currentLineIndent(codeEl.value, s);
      const closeLine = /^\s*\}/.test(afterRaw);
      if (before === "{" && closeLine) {
        e.preventDefault();
        replaceRange(s, s, "\n" + indent + "\t" + "\n" + indent);
        return;
      }
      e.preventDefault();
      replaceRange(s, s, "\n" + indent + (before === "{" ? "\t" : ""));
      return;
    }

    const PAIRS = { "(": ")", "[": "]", "{": "}", "'": "'", '"': '"', "`": "`" };
    if (PAIRS[e.key] && codeEl.selectionStart === codeEl.selectionEnd) {
      const next = codeEl.value[codeEl.selectionStart] || "";
      if (/[\w`"']/.test(next) && (e.key === "'" || e.key === '"' || e.key === "`")) return;
      e.preventDefault();
      replaceRange(codeEl.selectionStart, codeEl.selectionEnd, e.key + PAIRS[e.key],
        codeEl.selectionStart + 1);
      return;
    }
    if (/[)\]}'"`]/.test(e.key) && e.key === (codeEl.value[codeEl.selectionStart] || "") &&
        codeEl.selectionStart === codeEl.selectionEnd) {
      e.preventDefault(); // overtype the existing closer
      setSelection(codeEl.selectionStart + 1, codeEl.selectionStart + 1);
      return;
    }
    if (e.key === "Backspace" && codeEl.selectionStart === codeEl.selectionEnd) {
      const p = codeEl.selectionStart;
      const a = codeEl.value[p - 1], b = codeEl.value[p];
      if (a && b && PAIRS[a] === b) { e.preventDefault(); replaceRange(p - 1, p + 1, "", p - 1); }
    }
  });

  // ---------- draft persistence (A4) ----------
  let draftTimer = null;
  function saveDraft() {
    clearTimeout(draftTimer);
    draftTimer = setTimeout(() => {
      try {
        localStorage.setItem(DRAFT_KEY, codeEl.value);
        localStorage.setItem(STDIN_KEY, stdinEl.value);
        localStorage.setItem(DRAFT_AT_KEY, String(Date.now()));
        flashSaved();
      } catch { /* storage unavailable: drafting is best-effort */ }
    }, 400);
  }

  function flashSaved() {
    saveState.textContent = "已自动保存";
    clearTimeout(flashSaved.t);
    flashSaved.t = setTimeout(() => { saveState.textContent = ""; }, 1500);
  }

  function initDraft() {
    let draft = null, savedAt = 0;
    try {
      draft = localStorage.getItem(DRAFT_KEY);
      savedAt = parseInt(localStorage.getItem(DRAFT_AT_KEY) || "0", 10) || 0;
    } catch {}
    if (draft && draft !== codeEl.value) {
      if (savedAt > 0) {
        const d = new Date(savedAt);
        const hhmm = String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
        restoreWhen.textContent = "（保存于 " + hhmm + "）";
      } else {
        restoreWhen.textContent = "";
      }
      restoreBar.hidden = false;
      restoreYes.onclick = () => {
        codeEl.value = draft;
        try { stdinEl.value = localStorage.getItem(STDIN_KEY) || ""; } catch {}
        restoreBar.hidden = true;
        refreshEditor();
      };
      restoreNo.onclick = () => {
        try {
          localStorage.removeItem(DRAFT_KEY);
          localStorage.removeItem(DRAFT_AT_KEY);
          localStorage.removeItem(STDIN_KEY);
        } catch {}
        restoreBar.hidden = true;
      };
    }
  }

  // ---------- status rendering ----------
  // 徽章色按 LUOT-5 规范 §3.1 状态色表映射：
  // 蓝=等待 · 青=活跃 · 绿=成功 · 玫红=编译期 · 正红=运行期 ·
  // 琥珀=资源/容量受限 · 橙=平台拒绝 · 紫=内存超限（独立色相）
  const STATUS_STYLE = {
    idle: ["c-ready", "就绪"],
    queued: ["c-blue", "排队中"],
    running: ["c-cyan", "运行中"],
    success: ["c-ok", "运行成功"],
    compile_failed: ["c-rose", "编译失败"],
    run_failed: ["c-red", "运行失败"],
    timeout: ["c-amber", "运行超时"],
    memory_exceeded: ["c-violet", "内存超限"],
    queue_timeout: ["c-amber", "排队超时"],
    rejected: ["c-orange", "提交被拒绝"],
    cancelled: ["c-ready", "已停止"],
  };

  // 终态横幅呈现（具名状态 + 一句话原因 + 恢复建议，规范 §4.1）
  const STATUS_META = {
    compile_failed: { kind: "rose", icon: "octagon" },
    run_failed: { kind: "red", icon: "alert", note: "stderr 为程序自身输出，不是平台内部信息，请据此检查代码。" },
    timeout: { kind: "amber", icon: "clock", note: "已产生的输出照常展示，输出可能不完整。" },
    memory_exceeded: { kind: "violet", icon: "ram", note: "建议：降低内存占用，例如减小缓冲区或分批处理。" },
    queue_timeout: { kind: "amber", icon: "clock", note: "代码已保留在编辑器中，不会丢失。", action: { label: "重新排队", act: "rerun" } },
    rejected: { kind: "orange", icon: "ban", note: "平台不会自动重复提交，可稍后手动重试。", action: { label: "手动重试", act: "rerun" } },
    cancelled: { kind: "neutral", icon: "info" },
  };

  function setStatus(status, extra) {
    const def = STATUS_STYLE[status] || ["c-ready", status];
    statusChip.className = "chip " + def[0];
    statusChip.textContent = extra ? def[1] + " · " + extra : def[1];
  }

  function resetOutput() {
    noticeEl.hidden = true; queueInfo.hidden = true;
    stdoutSection.hidden = true; stderrSection.hidden = true;
    stdoutEl.textContent = ""; stderrEl.textContent = "";
    durationChip.hidden = true; traceChip.hidden = true;
    worklineEl.hidden = true;
    placeholderEl.hidden = true;
    state.outText = "";
    document.querySelectorAll(".trunc").forEach((n) => n.remove());
    clearErrMarks();
  }

  function fmtBytes(n) {
    if (n >= 1024) return (n / 1024).toFixed(1) + " KB";
    return n + " B";
  }

  function showTrace() {
    if (!state.lastTrace) return;
    traceChip.textContent = "trace " + state.lastTrace.slice(0, 12);
    traceChip.title = "run_id: " + state.lastTrace;
    traceChip.hidden = false;
  }

  function renderStream(pre, text, label) {
    pre.textContent = "";
    if (!text) return;
    const lines = text.split("\n");
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    const frag = document.createDocumentFragment();
    for (const t of lines) {
      const row = document.createElement("div"); row.className = "ol";
      const src = document.createElement("span"); src.className = "src"; src.textContent = label;
      const tx = document.createElement("span"); tx.className = "tx"; tx.textContent = t;
      row.append(src, tx);
      frag.appendChild(row);
    }
    pre.appendChild(frag);
    pre.hidden = false;
  }

  function renderTerminal(v) {
    setStatus(v.status, v.duration_ms ? (v.duration_ms / 1000).toFixed(2) + "s" : undefined);
    if (v.duration_ms) { durationChip.textContent = "耗时 " + (v.duration_ms / 1000).toFixed(2) + "s"; durationChip.hidden = false; }
    showTrace();
    worklineEl.hidden = true;

    // 终态横幅：success 不设横幅（输出即内容），其余按色族呈现
    const errs = v.compile_errors || [];
    const hasStructured = v.status === "compile_failed" && errs.length > 0;
    if (v.status !== "success") {
      const meta = STATUS_META[v.status] || {};
      const name = (STATUS_STYLE[v.status] || [, v.status])[1];
      renderBanner(noticeEl, {
        kind: meta.kind || "neutral",
        icon: meta.icon || "info",
        title: hasStructured ? name + " · " + errs.length + " 个错误" : name,
        body: v.reason || "",
        errors: hasStructured ? errs : undefined,
        note: meta.note,
        action: meta.action,
      });
    }

    if (hasStructured) markErrorLines(errs);
    if (v.truncated) {
      const trunc = document.createElement("div");
      trunc.className = "trunc";
      trunc.innerHTML = ICONS.warn;
      trunc.appendChild(document.createTextNode("输出超过上限，已截断（丢弃 " + fmtBytes(v.dropped_bytes) + "）"));
      stderrSection.after(trunc);
    }

    // 编译失败的结构化诊断在横幅内呈现，编译器原始 stderr 不再重复进输出流
    if (v.stdout) { renderStream(stdoutEl, v.stdout, "out"); stdoutSection.hidden = false; }
    if (v.stderr && !hasStructured) { renderStream(stderrEl, v.stderr, "err"); stderrSection.hidden = false; }
    state.outText = (v.stdout || "") + (hasStructured ? "" : v.stderr || "");
    if (!v.stdout && !v.stderr && v.status === "success") {
      renderStream(stdoutEl, "（程序没有输出）", "out");
      stdoutEl.firstElementChild.classList.add("muted");
      stdoutSection.hidden = false;
    }
  }

  function markErrorLines(errs) {
    errLines = new Set(errs.map((e) => e.line).filter((l) => l > 0));
    buildErrLayer();
  }

  function gotoLine(line) {
    const lines = codeEl.value.split("\n");
    let start = 0;
    for (let i = 0; i < line - 1 && i < lines.length; i++) start += lines[i].length + 1;
    const end = start + (lines[line - 1] || "").length;
    setSelection(start, end);

    const lh = parseFloat(getComputedStyle(codeEl).lineHeight) || 22;
    codeEl.scrollTop = Math.max(0, (line - 1) * lh - codeEl.clientHeight / 3);
    syncScroll();

    const gutterLine = gutterEl.children[line - 1];
    if (gutterLine) {
      gutterLine.classList.add("gutter-active");
      setTimeout(() => gutterLine.classList.remove("gutter-active"), 1800);
    }
    const backdrop = errLayerEl.children[line - 1];
    if (backdrop) {
      backdrop.classList.remove("line-flash");
      void backdrop.offsetWidth;
      backdrop.classList.add("line-flash");
      setTimeout(() => backdrop.classList.remove("line-flash"), 1700);
    }
  }

  // ---------- run / poll / cancel ----------
  // 运行按钮模式矩阵（规范 §5.3）：就绪/完成 → 运行/重新运行可点；
  // 排队中 → 禁用 + [取消]；运行中 → 禁用 + [停止]
  function setRunMode(mode) {
    state.mode = mode;
    state.busy = mode === "queued" || mode === "running";
    btnRun.disabled = state.busy;
    btnStop.hidden = !state.busy;
    btnStop.textContent = mode === "queued" ? "取消排队" : "停止";
    btnFormat.disabled = state.busy;
    const label = btnRun.firstElementChild;
    if (mode === "running") { label.innerHTML = SPIN_HTML + "<span>运行中…</span>"; }
    else if (mode === "queued") { label.textContent = "排队中…"; }
    else if (mode === "done") { label.textContent = "▶ 重新运行"; }
    else { label.textContent = "▶ 运行"; }
  }

  async function submitRun() {
    if (state.busy) return;
    const code = codeEl.value;
    if (!code.trim()) {
      setStatus("rejected", "代码为空");
      renderBanner(noticeEl, { kind: "orange", icon: "ban", title: "提交被拒绝", body: "代码为空，写点什么再运行吧。" });
      return;
    }
    const maxBytes = (META.code_max_kb || 64) * 1024;
    if (new TextEncoder().encode(code).length > maxBytes) {
      toast("rose", "代码超过 64 KB 上限", "请精简代码后运行（A1 前端校验，请求未发送）。");
      return;
    }

    resetOutput();
    setRunMode("queued");
    setStatus("queued", "提交中");

    try {
      const resp = await fetch("/api/run", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ code, stdin: stdinEl.value }),
      });
      const data = await resp.json();
      if (!resp.ok || data.status === "rejected") {
        setRunMode("idle");
        setStatus("rejected");
        renderBanner(noticeEl, {
          kind: "orange", icon: "ban", title: "提交被拒绝",
          body: data.reason || "提交被拒绝",
          note: "平台不会自动重复提交，可稍后手动重试。",
          action: { label: "手动重试", act: "rerun", cooldown: data.reason_code === "rate_limited" },
        });
        return;
      }
      state.lastTrace = data.run_id;
      state.runId = data.run_id;
      pollLoop();
    } catch {
      setRunMode("idle");
      setStatus("rejected");
      renderBanner(noticeEl, { kind: "red", icon: "alert", title: "网络错误", body: "无法连接服务，请检查网络后重试。" });
    }
  }

  function renderQueueBanner(v) {
    const maxc = META.max_concurrency || 3;
    const pos = v.queue_position || 1;
    const etaS = v.estimated_wait_ms ? Math.max(1, Math.round(v.estimated_wait_ms / 1000)) : 0;
    const waitedS = Math.floor((Date.now() - state.queueStart) / 1000);
    setStatus("queued", "第 " + pos + " 位" + (etaS ? " · 预计等待约 " + etaS + " 秒" : ""));
    renderBanner(queueInfo, {
      kind: "blue", icon: "queue",
      title: "排队中 · 第 " + pos + " 位",
      body: maxc + " 个运行槽位均已占用，" +
        (etaS ? "预计等待约 " + etaS + " 秒" : "预计等待计算中…") + "，排队期间可随时取消。",
      progress: etaS ? Math.min(100, Math.round(waitedS / etaS * 100)) : 0,
      progressLabel: etaS
        ? "已等待 " + waitedS + " s · 预计还需 " + Math.max(0, etaS - waitedS) + " s"
        : "已等待 " + waitedS + " s",
      action: { label: "取消排队", act: "cancel" },
    });
  }

  function pollLoop() {
    let delay = 550;
    const tick = async () => {
      if (!state.runId) return;
      let v;
      try {
        const resp = await fetch("/api/run/" + state.runId);
        if (resp.status === 404) {
          finishPoll({ status: "rejected", reason: "运行记录已过期（10 分钟未查看会被清理）" });
          return;
        }
        v = await resp.json();
        delay = 550;
      } catch {
        state.pollTimer = setTimeout(tick, Math.min((delay *= 1.4), 3000)); // transient network hiccup
        return;
      }

      if (v.status === "queued") {
        if (!state.queueStart) state.queueStart = Date.now();
        renderQueueBanner(v);
        placeholderEl.hidden = false; // 排队期间输出区保持空态（规范 §5.6）
        worklineEl.hidden = true;
        state.pollTimer = setTimeout(tick, delay);
        return;
      }
      if (v.status === "running") {
        setStatus("running");
        queueInfo.hidden = true;
        placeholderEl.hidden = true;
        worklineEl.hidden = false;
        if (!state.runStart) state.runStart = Date.now();
        durationChip.hidden = false;
        durationChip.textContent = "已用时 " + ((Date.now() - state.runStart) / 1000).toFixed(1) + "s";
        state.pollTimer = setTimeout(tick, delay);
        return;
      }
      finishPoll(v);
    };
    state.pollTimer = setTimeout(tick, 250);
  }

  function finishPoll(v) {
    clearTimeout(state.pollTimer);
    state.runId = null;
    state.queueStart = 0;
    state.runStart = 0;
    queueInfo.hidden = true;
    renderTerminal(v);
    setRunMode("done");
  }

  async function stopRun() {
    if (!state.runId) return;
    try { await fetch("/api/run/" + state.runId, { method: "DELETE" }); } catch {}
    setStatus("cancelled", "正在停止…");
  }

  // ---------- format (A2) ----------
  async function formatCode() {
    const resp = await fetch("/api/format", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code: codeEl.value }),
    });
    const data = await resp.json();
    if (!data.ok) {
      const m = /^(?:.*main\.go?:)?(\d+):\d+:\s*/.exec(data.error || "");
      toast("rose", "格式化失败", "代码保持原样：" + (data.error || "语法错误"));
      if (m && m[1]) gotoLine(parseInt(m[1], 10));
      return;
    }
    const pos = codeEl.selectionStart;
    codeEl.value = data.formatted;
    refreshEditor(); saveDraft();
    toast("ok", "已格式化", "已按 gofmt 风格整理。");
  }

  // ---------- health & meta ----------
  let META = {};

  function fmtGoVer(v) {
    const m = /go(\d+\.\d+(?:\.\d+)?)/.exec(v || "");
    return m ? "Go " + m[1] : v;
  }

  async function refreshHealth() {
    try {
      const h = await (await fetch("/api/health")).json();
      // 顶栏容量指示：3 槽位圆点（实心=占用）+ 计数，数据来自 /api/health
      if (capEl.dataset.max !== String(h.max_running)) {
        capEl.dataset.max = String(h.max_running);
        capEl.querySelectorAll(".d").forEach((d) => d.remove());
        for (let i = 0; i < h.max_running; i++) {
          const d = document.createElement("span");
          d.className = "d";
          capEl.insertBefore(d, queueChip);
        }
      }
      capEl.querySelectorAll(".d").forEach((d, i) => d.classList.toggle("busy", i < h.running));
      queueChip.textContent = h.running + "/" + h.max_running;
      capEl.title = "运行槽位 " + h.running + "/" + h.max_running + " · 队列等待 " + h.queued;
      capEl.setAttribute("aria-label", capEl.title);
      if (h.runner === "local" && !capEl.dataset.localWarned) {
        capEl.dataset.localWarned = "1";
        const warn = document.createElement("span");
        warn.className = "go-chip chip-warn";
        warn.title = "当前未接入 Docker，代码在服务器本机受限执行：无网络隔离。";
        warn.textContent = "⚠ 开发执行器";
        goversionChip.after(warn);
      }
    } catch { /* health is cosmetic */ }
  }

  async function loadMeta() {
    try { META = await (await fetch("/api/meta")).json(); } catch { return; }
    if (META.go_version) {
      goversionChip.hidden = false;
      goversionChip.textContent = fmtGoVer(META.go_version);
      goversionChip.title = "沙箱 Go 版本：" + META.go_version;
    }
    limitsNote.textContent = "单文件 · 仅 Go 标准库 · CPU " + META.cpu_cores + " 核 · " + META.memory_mb +
      "MB · ≤" + META.exec_timeout_s + "s · 并发 " + META.max_concurrency;
  }

  // ---------- wire up ----------
  btnRun.addEventListener("click", submitRun);
  btnStop.addEventListener("click", stopRun);
  btnFormat.addEventListener("click", formatCode);
  btnClear.addEventListener("click", () => {
    resetOutput();
    setStatus("idle");
    placeholderEl.hidden = false;
    setRunMode("idle");
  });
  btnStdin.addEventListener("click", () => {
    stdinWrap.hidden = !stdinWrap.hidden;
    btnStdin.setAttribute("aria-expanded", String(!stdinWrap.hidden));
    btnStdin.classList.toggle("on", !stdinWrap.hidden);
    btnStdin.textContent = stdinWrap.hidden ? "stdin ▾" : "stdin ▴";
  });
  btnReset.addEventListener("click", () => {
    codeEl.value = DEFAULT_TEMPLATE;
    clearErrMarks();
    refreshEditor(); saveDraft();
    toast("ok", "已重置", "已恢复为 Hello World 示例。");
  });
  btnCopy.addEventListener("click", () => {
    const text = state.outText;
    if (!text) return;
    const done = () => toast("ok", "已复制", "输出内容已复制到剪贴板。");
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, () => {});
    } else {
      const t = document.createElement("textarea");
      t.value = text;
      document.body.appendChild(t);
      t.select();
      document.execCommand("copy");
      t.remove();
      done();
    }
  });

  // 横幅内动作按钮：取消排队/停止、重新运行、手动重试（限流拒绝带 10s 冷却）
  function handleBannerAction(btn) {
    const act = btn.dataset.act;
    if (act === "cancel") stopRun();
    else if (act === "rerun") {
      if (btn.dataset.cooldown === "1" || btn.dataset.cooldown === "true") {
        let left = 10;
        const label = btn.textContent;
        btn.disabled = true;
        const iv = setInterval(() => {
          left--;
          btn.textContent = label + "（" + left + "s）";
          if (left <= 0) { clearInterval(iv); btn.disabled = false; submitRun(); }
        }, 1000);
        btn.textContent = label + "（" + left + "s）";
      } else {
        submitRun();
      }
    }
  }
  noticeEl.addEventListener("click", (e) => {
    const bt = e.target.closest(".bbtn");
    if (bt) handleBannerAction(bt);
  });
  queueInfo.addEventListener("click", (e) => {
    const bt = e.target.closest(".bbtn");
    if (bt) handleBannerAction(bt);
  });

  // 隐私说明浮层（页脚声明的完整版）
  infoBtn.addEventListener("click", () => {
    const open = infoPop.hidden;
    infoPop.hidden = !open;
    infoBtn.setAttribute("aria-expanded", String(open));
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest("#info-pop") && !e.target.closest("#info-btn")) infoPop.hidden = true;
  });
  $("status-page-link").addEventListener("click", (e) => e.preventDefault());

  codeEl.value = DEFAULT_TEMPLATE;
  refreshEditor();
  initDraft();
  loadMeta();
  refreshHealth();
  setInterval(refreshHealth, 5000);
  codeEl.addEventListener("input", () => { clearErrMarks(); saveDraft(); });
  stdinEl.addEventListener("input", saveDraft);
  codeEl.addEventListener("keyup", updateCursorInfo);
  codeEl.addEventListener("click", updateCursorInfo);
})();
