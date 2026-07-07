// editor.js — live Markdown preview for the notes editor. Deliberately tiny:
// this is an approximate preview so "# boom" shows as a big heading while you
// type. The authoritative render is Quartz on publish, so this only needs to
// cover the everyday cases (headings, bold/italic, inline code, links, lists,
// fenced code) — not be a full CommonMark engine.
(function () {
  "use strict";
  var src = document.getElementById("md-src");
  var out = document.getElementById("md-preview");
  if (!src || !out) return;

  function esc(s) {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  function inline(s) {
    return esc(s)
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/\*([^*]+)\*/g, "<em>$1</em>")
      .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<a href="$2">$1</a>');
  }

  function render(md) {
    var lines = md.split("\n");
    var html = "";
    var inCode = false;
    var inList = false;
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (line.trim().indexOf("```") === 0) {
        if (inList) { html += "</ul>"; inList = false; }
        inCode = !inCode;
        html += inCode ? "<pre><code>" : "</code></pre>";
        continue;
      }
      if (inCode) { html += esc(line) + "\n"; continue; }

      var h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) {
        if (inList) { html += "</ul>"; inList = false; }
        var n = h[1].length;
        html += "<h" + n + ">" + inline(h[2]) + "</h" + n + ">";
        continue;
      }
      var li = line.match(/^\s*[-*]\s+(.*)$/);
      if (li) {
        if (!inList) { html += "<ul>"; inList = true; }
        html += "<li>" + inline(li[1]) + "</li>";
        continue;
      }
      if (inList) { html += "</ul>"; inList = false; }
      if (line.trim() === "") continue;
      html += "<p>" + inline(line) + "</p>";
    }
    if (inList) html += "</ul>";
    if (inCode) html += "</code></pre>";
    return html;
  }

  function update() { out.innerHTML = render(src.value); }
  src.addEventListener("input", update);
  update();
})();
