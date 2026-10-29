/**
 * Strict `$...$` inline math for chat markdown.
 *
 * remark-math runs with singleDollarTextMath disabled, so only `$$...$$` is
 * math out of the box. remarkStrictInlineMath then walks the parsed mdast
 * and turns a single-dollar pair inside plain text into an inlineMath node
 * only when it looks like math under pandoc's rules:
 *   - the opening `$` is immediately followed by a non-space character
 *   - the closing `$` is immediately preceded by a non-space character
 *   - the closing `$` is not immediately followed by a digit
 *   - both delimiters sit on the same line and are not backslash-escaped
 * so prose such as "$5 and $10" stays plain text. Because it works on text
 * nodes, code (fenced, indented, inline) and link destinations are never
 * touched.
 */

interface Node {
  type: string;
  value?: string;
  children?: Node[];
  position?: { start: { offset?: number }; end: { offset?: number } };
  data?: Record<string, unknown>;
}

const isSpace = (c: string | undefined) => c === undefined || /\s/.test(c);
const isDigit = (c: string | undefined) => c !== undefined && c >= "0" && c <= "9";

/** Splits raw markdown text into text / math segments. Exported for tests. */
export function splitStrictInlineMath(raw: string): { math: boolean; value: string }[] {
  const out: { math: boolean; value: string }[] = [];
  let buf = "";
  let i = 0;
  while (i < raw.length) {
    const c = raw[i];
    if (c === "\\") {
      buf += raw.slice(i, i + 2);
      i += 2;
      continue;
    }
    if (c === "$") {
      const close = findClose(raw, i);
      if (close !== -1) {
        if (buf) out.push({ math: false, value: buf });
        out.push({ math: true, value: raw.slice(i + 1, close) });
        buf = "";
        i = close + 1;
        continue;
      }
    }
    buf += c;
    i++;
  }
  if (buf) out.push({ math: false, value: buf });
  return out;
}

function findClose(s: string, open: number): number {
  if (s[open - 1] === "$" || s[open + 1] === "$" || isSpace(s[open + 1])) return -1;
  for (let j = open + 1; j < s.length; j++) {
    const c = s[j];
    if (c === "\\") {
      j++;
      continue;
    }
    if (c === "\n") return -1;
    if (c !== "$") continue;
    if (s[j + 1] !== "$" && !isSpace(s[j - 1]) && !isDigit(s[j + 1])) return j;
    // An unqualified `$` ends the candidate span: "$5 and $10" stays text.
    return -1;
  }
  return -1;
}

const ESCAPE_RE = /\\([!-/:-@[-`{-~])/g;
const unescape = (s: string) => s.replace(ESCAPE_RE, "$1");

function inlineMath(value: string): Node {
  return {
    type: "inlineMath",
    value,
    data: {
      hName: "code",
      hProperties: { className: ["language-math", "math-inline"] },
      hChildren: [{ type: "text", value }],
    },
  };
}

export function remarkStrictInlineMath() {
  return (tree: Node, file: { value?: unknown }) => {
    const src = typeof file.value === "string" ? file.value : String(file.value ?? "");
    const walk = (node: Node) => {
      const kids = node.children;
      if (!kids) return;
      for (let i = 0; i < kids.length; i++) {
        const k = kids[i];
        if (k.type !== "text") {
          walk(k);
          continue;
        }
        const s = k.position?.start.offset;
        const e = k.position?.end.offset;
        // Work on the source slice so backslash escapes are still visible.
        const raw = s !== undefined && e !== undefined ? src.slice(s, e) : null;
        // Skip nodes with character references: re-deriving their text from
        // source would leave "&amp;" undecoded.
        if (raw === null || !raw.includes("$") || raw.includes("&")) continue;
        const parts = splitStrictInlineMath(raw);
        if (!parts.some((p) => p.math)) continue;
        const repl = parts.map((p) =>
          p.math ? inlineMath(p.value) : ({ type: "text", value: unescape(p.value) } as Node),
        );
        kids.splice(i, 1, ...repl);
        i += repl.length - 1;
      }
    };
    walk(tree);
  };
}
