"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const markdown = require("./markdown.js");

test("renders the common Markdown blocks used by Codex", () => {
  const html = markdown.render([
    "# 标题",
    "",
    "> 引用里的 **重点**",
    "",
    "3. 第三项",
    "4. 第四项",
    "   - 子项",
    "",
    "- [x] 已完成",
    "- [ ] 待处理",
    "",
    "| 文件 | 状态 |",
    "|:---|---:|",
    "| `app.js` | **通过** |",
    "",
    "```js",
    "const value = '<safe>';",
    "```",
    "",
    "---",
  ].join("\n"));

  assert.match(html, /<h1>标题<\/h1>/);
  assert.match(html, /<blockquote>[\s\S]*<strong>重点<\/strong>[\s\S]*<\/blockquote>/);
  assert.match(html, /<ol start="3">/);
  assert.match(html, /<ul><li>子项<\/li><\/ul>/);
  assert.match(html, /class="md-task-list"/);
  assert.match(html, /type="checkbox" disabled checked/);
  assert.match(html, /class="md-table-wrap"/);
  assert.match(html, /class="md-align-right"/);
  assert.match(html, /class="language-js" data-language="js"/);
  assert.match(html, /&lt;safe&gt;/);
  assert.match(html, /<hr>/);
});

test("renders inline emphasis, code, links, autolinks and hard breaks", () => {
  const html = markdown.render([
    "**粗体**、*斜体*、~~删除~~、`a < b`  ",
    "下一行 [官网](https://example.com/path?q=1 \"说明\") <https://openai.com>",
  ].join("\n"));

  assert.match(html, /<strong>粗体<\/strong>/);
  assert.match(html, /<em>斜体<\/em>/);
  assert.match(html, /<del>删除<\/del>/);
  assert.match(html, /<code>a &lt; b<\/code>/);
  assert.match(html, /<br>/);
  assert.match(html, /href="https:\/\/example\.com\/path\?q=1"/);
  assert.match(html, /title="说明"/);
  assert.match(html, /href="https:\/\/openai\.com"/);
  assert.match(html, /rel="noopener noreferrer"/);
});

test("escapes raw HTML and blocks active or local navigation schemes", () => {
  const html = markdown.render([
    "<img src=x onerror=alert(1)>",
    "[脚本](javascript:alert(1))",
    "[数据](data:text/html,<script>alert(1)</script>)",
    "[本地文件](/root/secret.txt)",
  ].join("\n\n"));

  assert.doesNotMatch(html, /<img src=/);
  assert.doesNotMatch(html, /href="javascript:/);
  assert.doesNotMatch(html, /href="data:/);
  assert.doesNotMatch(html, /href="\/root/);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /class="md-local-path" data-copy-path="\/root\/secret\.txt"/);
  assert.equal((html.match(/md-unsafe-link/g) || []).length, 2);
});

test("only resolves local Markdown images through the artifact endpoint", () => {
  const html = markdown.render("![截图](/opt/project/result.png) ![远端](https://tracker.example/pixel.png)", {
    resolveImage(destination) {
      return destination.startsWith("/opt/project/") ? `/api/artifact?path=${encodeURIComponent(destination)}` : "";
    },
  });

  assert.match(html, /class="md-image"/);
  assert.match(html, /data-artifact-image/);
  assert.match(html, /src="\/api\/artifact\?path=%2Fopt%2Fproject%2Fresult\.png"/);
  assert.match(html, /class="md-image-link"/);
  assert.doesNotMatch(html, /<img[^>]+tracker\.example/);
});

test("keeps unsupported Markdown extensions readable instead of emitting HTML", () => {
  const source = "Term\n: definition\n\n[^1]: footnote\n\n~~unfinished";
  const html = markdown.render(source);
  assert.match(html, /Term/);
  assert.match(html, /: definition/);
  assert.match(html, /\[\^1\]: footnote/);
  assert.match(html, /~~unfinished/);
});

test("renders every streaming prefix without producing active HTML", () => {
  const stream = "# 标题\n\n- **逐步生成**\n\n```html\n<script>alert(1)</script>\n```\n\n[bad](JaVaScRiPt:alert(1))";
  for (let length = 0; length <= stream.length; length += 1) {
    const html = markdown.render(stream.slice(0, length), { breaks: true });
    assert.doesNotMatch(html, /<script[\s>]/i);
    assert.doesNotMatch(html, /href="javascript:/i);
  }
});
