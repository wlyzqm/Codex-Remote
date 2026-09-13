(function attachMarkdown(root, factory) {
  "use strict";

  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.CodexRemoteMarkdown = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function createMarkdownRenderer() {
  "use strict";

  const TOKEN_OPEN = "\uE000md";
  const TOKEN_CLOSE = "\uE001";
  const INLINE_PUNCTUATION = /\\([\\`*{}\[\]()#+\-.!_|>~])/g;

  function escapeHtml(value) {
    return String(value ?? "").replace(/[&<>'"]/g, (character) => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      "'": "&#39;",
      '"': "&quot;",
    })[character]);
  }

  function token(index) {
    return `${TOKEN_OPEN}${index}${TOKEN_CLOSE}`;
  }

  function restoreTokens(value, tokens) {
    const pattern = new RegExp(`${TOKEN_OPEN}(\\d+)${TOKEN_CLOSE}`, "g");
    let result = value;
    for (let pass = 0; pass <= tokens.length; pass += 1) {
      const next = result.replace(pattern, (_match, index) => tokens[Number(index)] ?? "");
      if (next === result) break;
      result = next;
    }
    return result;
  }

  function safeLinkTarget(value) {
    const href = String(value || "").trim();
    if (/^https?:\/\/[^\s]+$/i.test(href) || /^mailto:[^\s@]+@[^\s@]+$/i.test(href) || /^#[A-Za-z0-9_:.~-]+$/.test(href)) return href;
    return "";
  }

  function linkHtml(label, destination, title) {
    const localPath = String(destination || "").trim();
    if (/^\/[^\s<>"']+(?::\d+)?$/.test(localPath)) {
      return `<button type="button" class="md-local-path" data-copy-path="${escapeHtml(localPath)}"${title ? ` title="${escapeHtml(title)}"` : ' title="复制本地路径"'}>${label}</button>`;
    }
    const href = safeLinkTarget(destination);
    if (!href) return `<span class="md-unsafe-link" title="链接地址未开放">${label}</span>`;
    const external = /^https?:\/\//i.test(href);
    const attributes = [
      `href="${escapeHtml(href)}"`,
      external ? 'target="_blank"' : "",
      external ? 'rel="noopener noreferrer"' : "",
      title ? `title="${escapeHtml(title)}"` : "",
    ].filter(Boolean).join(" ");
    return `<a ${attributes}>${label}</a>`;
  }

  function imageHtml(alt, destination, title, options) {
    const resolved = typeof options.resolveImage === "function" ? String(options.resolveImage(destination) || "") : "";
    if (/^\/api\/artifact\?[^\s]+$/.test(resolved)) {
      const caption = escapeHtml(alt || "Markdown 图像");
      return `<a class="md-image" href="${escapeHtml(resolved)}" target="_blank" rel="noopener noreferrer"${title ? ` title="${escapeHtml(title)}"` : ""}><img data-artifact-image src="${escapeHtml(resolved)}" alt="${caption}" loading="lazy" decoding="async"><span>${caption}</span></a>`;
    }
    const href = safeLinkTarget(destination);
    if (href && /^https?:\/\//i.test(href)) return `<span class="md-image-link">图像：${linkHtml(escapeHtml(alt || destination), href, title)}</span>`;
    return `<span class="md-image-link md-unsafe-link">图像：${escapeHtml(alt || destination || "不可用")}</span>`;
  }

  function renderInline(value, options = {}) {
    let source = String(value ?? "");
    const tokens = [];
    const stash = (html) => {
      const index = tokens.push(html) - 1;
      return token(index);
    };

    source = source.replace(/(`+)([\s\S]*?)\1(?!`)/g, (_match, _ticks, content) => {
      let code = content.replace(/\n/g, " ");
      if (/^\s[\s\S]*\s$/.test(code) && /\S/.test(code)) code = code.slice(1, -1);
      return stash(`<code>${escapeHtml(code)}</code>`);
    });

    // A streamed link may lack its closing ')'; nested repetition here blocks the UI.
    const markdownLink = /(!?)\[([^\]\n]*)\]\(\s*(<[^>\n]+>|(?:[^()\s]|\([^()\s]*\))+)(?:\s+(?:"([^"]*)"|'([^']*)'|\(([^)]*)\)))?\s*\)/g;
    source = source.replace(markdownLink, (_match, image, label, rawDestination, titleA, titleB, titleC) => {
      const destination = rawDestination.startsWith("<") && rawDestination.endsWith(">") ? rawDestination.slice(1, -1) : rawDestination;
      const title = titleA || titleB || titleC || "";
      if (image) return stash(imageHtml(label, destination, title, options));
      const renderedLabel = restoreTokens(renderInline(label, options), tokens);
      return stash(linkHtml(renderedLabel, destination, title));
    });

    source = source.replace(/<(https?:\/\/[^\s<>]+|mailto:[^\s<>]+)>/gi, (_match, destination) => stash(linkHtml(escapeHtml(destination.replace(/^mailto:/i, "")), destination, "")));
    source = source.replace(/ {2,}\n|\\\n/g, () => stash("<br>\n"));
    source = source.replace(INLINE_PUNCTUATION, (_match, character) => stash(escapeHtml(character)));
    if (options.breaks) source = source.replace(/\n/g, () => stash("<br>\n"));

    source = source.replace(/\bhttps?:\/\/[^\s<>]+/gi, (candidate) => {
      const punctuation = candidate.match(/[),.;:!?]+$/)?.[0] || "";
      const destination = punctuation ? candidate.slice(0, -punctuation.length) : candidate;
      return `${stash(linkHtml(escapeHtml(destination), destination, ""))}${punctuation}`;
    });

    let html = escapeHtml(source);
    html = html
      .replace(/~~([^~\n]+)~~/g, "<del>$1</del>")
      .replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>")
      .replace(/__([^_\n]+)__/g, "<strong>$1</strong>")
      .replace(/(^|[^\w])\*([^*\n]+)\*(?!\w)/g, "$1<em>$2</em>")
      .replace(/(^|[^\w])_([^_\n]+)_(?!\w)/g, "$1<em>$2</em>");
    return restoreTokens(html, tokens);
  }

  function splitTableRow(line) {
    let source = String(line || "").trim();
    if (source.startsWith("|")) source = source.slice(1);
    if (source.endsWith("|") && !source.endsWith("\\|")) source = source.slice(0, -1);
    const cells = [];
    let cell = "";
    let escaped = false;
    let tickRun = 0;
    for (let index = 0; index < source.length; index += 1) {
      const character = source[index];
      if (escaped) {
        cell += character;
        escaped = false;
      } else if (character === "\\") {
        escaped = true;
        cell += character;
      } else if (character === "`") {
        tickRun = tickRun ? 0 : 1;
        cell += character;
      } else if (character === "|" && !tickRun) {
        cells.push(cell.trim());
        cell = "";
      } else {
        cell += character;
      }
    }
    cells.push(cell.trim());
    return cells;
  }

  function tableDelimiter(line) {
    const cells = splitTableRow(line);
    if (cells.length < 2 || !cells.every((cell) => /^:?-{3,}:?$/.test(cell.trim()))) return null;
    return cells.map((cell) => {
      const value = cell.trim();
      if (value.startsWith(":") && value.endsWith(":")) return "center";
      if (value.endsWith(":")) return "right";
      if (value.startsWith(":")) return "left";
      return "";
    });
  }

  function listMarker(line) {
    const match = String(line || "").match(/^(\s{0,12})([-+*]|\d+[.)])\s+([\s\S]*)$/);
    if (!match) return null;
    const ordered = /^\d/.test(match[2]);
    return {
      indent: match[1].length,
      ordered,
      start: ordered ? Number.parseInt(match[2], 10) : 1,
      content: match[3],
    };
  }

  function fenceMarker(line) {
    const match = String(line || "").match(/^\s{0,3}(`{3,}|~{3,})\s*([^`]*)$/);
    if (!match) return null;
    return { marker: match[1][0], length: match[1].length, info: match[2].trim() };
  }

  function isHorizontalRule(line) {
    return /^\s{0,3}((\*\s*){3,}|(-\s*){3,}|(_\s*){3,})$/.test(String(line || ""));
  }

  function isBlockStart(lines, index) {
    const line = lines[index] || "";
    if (!line.trim()) return true;
    if (fenceMarker(line) || /^\s{0,3}#{1,6}\s+/.test(line) || /^\s{0,3}>/.test(line) || isHorizontalRule(line)) return true;
    const marker = listMarker(line);
    if (marker && marker.indent <= 3) return true;
    if (index + 1 < lines.length && tableDelimiter(lines[index + 1]) && splitTableRow(line).length >= 2) return true;
    if (index + 1 < lines.length && /^\s*(=+|-+)\s*$/.test(lines[index + 1]) && line.trim()) return true;
    return false;
  }

  function unwrapSingleParagraph(html) {
    const match = html.trim().match(/^<p>([\s\S]*)<\/p>$/);
    return match ? match[1] : html;
  }

  function parseList(lines, start, options) {
    const first = listMarker(lines[start]);
    const baseIndent = first.indent;
    const ordered = first.ordered;
    const items = [];
    let current = null;
    let index = start;

    while (index < lines.length) {
      const line = lines[index];
      const marker = listMarker(line);
      if (marker && marker.indent === baseIndent && marker.ordered === ordered) {
        current = { lines: [marker.content] };
        items.push(current);
        index += 1;
        continue;
      }
      if (!current) break;
      if (!line.trim()) {
        const next = lines[index + 1];
        if (next === undefined) {
          index += 1;
          break;
        }
        const nextMarker = listMarker(next);
        const nextIndent = next.match(/^\s*/)?.[0].length || 0;
        if ((nextMarker && nextMarker.indent >= baseIndent) || nextIndent > baseIndent) {
          current.lines.push("");
          index += 1;
          continue;
        }
        index += 1;
        break;
      }
      const indentation = line.match(/^\s*/)?.[0].length || 0;
      if (indentation > baseIndent) {
        current.lines.push(line.slice(Math.min(line.length, baseIndent + 2)));
        index += 1;
        continue;
      }
      if (!isBlockStart(lines, index)) {
        current.lines.push(line.trimStart());
        index += 1;
        continue;
      }
      break;
    }

    const tag = ordered ? "ol" : "ul";
    const startAttribute = ordered && first.start !== 1 ? ` start="${first.start}"` : "";
    const renderedItems = items.map((item) => {
      const itemLines = [...item.lines];
      const task = itemLines[0].match(/^\[([ xX])\]\s+([\s\S]*)$/);
      if (task) itemLines[0] = task[2];
      let body = render(itemLines.join("\n"), options).trim();
      const checkbox = task ? `<input type="checkbox" disabled${task[1].toLowerCase() === "x" ? " checked" : ""} aria-label="${task[1].toLowerCase() === "x" ? "已完成" : "未完成"}">` : "";
      if (checkbox && body.startsWith("<p>")) body = body.replace("<p>", `<p>${checkbox}`);
      else body = `${checkbox}${unwrapSingleParagraph(body)}`;
      return `<li${task ? ' class="md-task-item"' : ""}>${body}</li>`;
    }).join("");
    return { html: `<${tag}${startAttribute}${items.some((item) => /^\[[ xX]\]\s+/.test(item.lines[0])) ? ' class="md-task-list"' : ""}>${renderedItems}</${tag}>`, next: index };
  }

  function render(value, options = {}) {
    const source = String(value ?? "").replace(/\r\n?/g, "\n");
    if (!source.trim()) return "";
    const lines = source.split("\n");
    const blocks = [];
    let index = 0;

    while (index < lines.length) {
      const line = lines[index];
      if (!line.trim()) {
        index += 1;
        continue;
      }

      const fence = fenceMarker(line);
      if (fence) {
        const content = [];
        index += 1;
        while (index < lines.length && !new RegExp(`^\\s{0,3}${fence.marker}{${fence.length},}\\s*$`).test(lines[index])) {
          content.push(lines[index]);
          index += 1;
        }
        if (index < lines.length) index += 1;
        const language = fence.info.split(/\s+/)[0].replace(/[^A-Za-z0-9_+-]/g, "").slice(0, 32);
        blocks.push(`<pre class="md-code-block"><code${language ? ` class="language-${escapeHtml(language)}" data-language="${escapeHtml(language)}"` : ""}>${escapeHtml(content.join("\n"))}</code></pre>`);
        continue;
      }

      if (/^(?: {4}|\t)/.test(line)) {
        const content = [];
        while (index < lines.length && (/^(?: {4}|\t)/.test(lines[index]) || !lines[index].trim())) {
          content.push(lines[index].replace(/^(?: {4}|\t)/, ""));
          index += 1;
        }
        while (content.length && !content[content.length - 1]) content.pop();
        blocks.push(`<pre class="md-code-block"><code>${escapeHtml(content.join("\n"))}</code></pre>`);
        continue;
      }

      const heading = line.match(/^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/);
      if (heading) {
        const level = heading[1].length;
        blocks.push(`<h${level}>${renderInline(heading[2], options)}</h${level}>`);
        index += 1;
        continue;
      }

      if (index + 1 < lines.length && /^\s*(=+|-+)\s*$/.test(lines[index + 1]) && line.trim()) {
        const level = lines[index + 1].includes("=") ? 1 : 2;
        blocks.push(`<h${level}>${renderInline(line.trim(), options)}</h${level}>`);
        index += 2;
        continue;
      }

      if (isHorizontalRule(line)) {
        blocks.push("<hr>");
        index += 1;
        continue;
      }

      if (/^\s{0,3}>/.test(line)) {
        const quote = [];
        while (index < lines.length) {
          const match = lines[index].match(/^\s{0,3}> ?(.*)$/);
          if (!match) break;
          quote.push(match[1]);
          index += 1;
        }
        blocks.push(`<blockquote>${render(quote.join("\n"), options)}</blockquote>`);
        continue;
      }

      const delimiter = index + 1 < lines.length ? tableDelimiter(lines[index + 1]) : null;
      const headerCells = splitTableRow(line);
      if (delimiter && headerCells.length === delimiter.length) {
        index += 2;
        const rows = [];
        while (index < lines.length && lines[index].trim() && lines[index].includes("|")) {
          const cells = splitTableRow(lines[index]);
          while (cells.length < headerCells.length) cells.push("");
          rows.push(cells.slice(0, headerCells.length));
          index += 1;
        }
        const cellClass = (column) => delimiter[column] ? ` class="md-align-${delimiter[column]}"` : "";
        blocks.push(`<div class="md-table-wrap"><table><thead><tr>${headerCells.map((cell, column) => `<th${cellClass(column)}>${renderInline(cell, options)}</th>`).join("")}</tr></thead><tbody>${rows.map((cells) => `<tr>${cells.map((cell, column) => `<td${cellClass(column)}>${renderInline(cell, options)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`);
        continue;
      }

      const marker = listMarker(line);
      if (marker && marker.indent <= 3) {
        const list = parseList(lines, index, options);
        blocks.push(list.html);
        index = list.next;
        continue;
      }

      const paragraph = [line.trimStart()];
      index += 1;
      while (index < lines.length && lines[index].trim() && !isBlockStart(lines, index)) {
        paragraph.push(lines[index].trimStart());
        index += 1;
      }
      blocks.push(`<p>${renderInline(paragraph.join("\n"), options)}</p>`);
    }

    return blocks.join("\n");
  }

  return Object.freeze({ render, renderInline, safeLinkTarget, escapeHtml });
});
