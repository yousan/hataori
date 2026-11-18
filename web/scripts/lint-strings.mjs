// lint-strings fails when a user-visible string is written anywhere but in
// the catalog (src/catalog.ts). It reads the page's sources with the
// TypeScript parser and reports:
//
//   - text between JSX tags, and a string or template written as a JSX child;
//   - a string or template given to an attribute people see or hear
//     (title, alt, placeholder, label, aria-label and the like);
//   - a string or template assigned to document.title, textContent,
//     innerText, innerHTML or outerHTML, or passed to alert, confirm or
//     prompt;
//   - text and those attributes in index.html.
//
// A string from the catalog is t("key"): the key is not user-visible text.
//
// Usage: node scripts/lint-strings.mjs [--expect-fail] [file...]
// With no files it checks src/ and index.html. With --expect-fail it exits
// non-zero unless it reports exactly the lines of the files given that are
// marked "want" (in a comment: // want, /* want */ or <!-- want -->); CI uses this to check that the lint still
// catches what it should, and only that.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import ts from "typescript";

const root = resolve(import.meta.dirname, "..");
const catalog = join(root, "src", "catalog.ts");

const visibleAttributes = new Set([
  "title",
  "alt",
  "placeholder",
  "label",
  "aria-label",
  "aria-description",
  "aria-placeholder",
  "aria-roledescription",
  "aria-valuetext",
]);
const visibleProperties = new Set(["title", "textContent", "innerText", "innerHTML", "outerHTML"]);
const dialogs = new Set(["alert", "confirm", "prompt"]);

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

function isText(node) {
  return ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node) || ts.isTemplateExpression(node);
}

// texts returns the string literals in an expression that would end up
// shown: the expression itself, or a branch of ?:, || or ??.
function texts(node) {
  if (!node) return [];
  if (ts.isParenthesizedExpression(node)) return texts(node.expression);
  if (ts.isJsxExpression(node)) return texts(node.expression);
  if (isText(node)) return /\S/.test(node.getText().slice(1, -1)) ? [node] : [];
  if (ts.isConditionalExpression(node)) return [...texts(node.whenTrue), ...texts(node.whenFalse)];
  if (ts.isBinaryExpression(node)) {
    const op = node.operatorToken.kind;
    if (op === ts.SyntaxKind.BarBarToken || op === ts.SyntaxKind.QuestionQuestionToken || op === ts.SyntaxKind.PlusToken) {
      return [...texts(node.left), ...texts(node.right)];
    }
  }
  return [];
}

function lintSource(file) {
  const found = [];
  const src = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true,
    file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  const report = (node, what) => {
    const { line, character } = src.getLineAndCharacterOfPosition(node.getStart(src));
    found.push(`${relative(root, file)}:${line + 1}:${character + 1}: ${what} outside the catalog: ${node.getText(src).trim().slice(0, 60)}`);
  };
  const visit = (node) => {
    if (ts.isJsxText(node) && /\S/.test(node.getText(src))) {
      report(node, "text");
    } else if (ts.isJsxExpression(node) && node.parent && (ts.isJsxElement(node.parent) || ts.isJsxFragment(node.parent))) {
      for (const s of texts(node)) report(s, "text");
    } else if (ts.isJsxAttribute(node) && visibleAttributes.has(node.name.getText(src)) && node.initializer) {
      for (const s of texts(node.initializer)) report(s, `attribute ${node.name.getText(src)}`);
    } else if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken &&
      ts.isPropertyAccessExpression(node.left) && visibleProperties.has(node.left.name.text)) {
      for (const s of texts(node.right)) report(s, node.left.name.text);
    } else if (ts.isCallExpression(node)) {
      const callee = ts.isPropertyAccessExpression(node.expression) ? node.expression.name : node.expression;
      if (ts.isIdentifier(callee) && dialogs.has(callee.text)) {
        for (const a of node.arguments) for (const s of texts(a)) report(s, callee.text);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(src);
  return found;
}

// lintHTML checks the page's HTML: no text and no visible attribute value.
function lintHTML(file) {
  const found = [];
  const html = readFileSync(file, "utf8");
  const at = (i) => `${relative(root, file)}:${html.slice(0, i).split("\n").length}`;
  const stripped = html.replace(/<!--[\s\S]*?-->/g, (m) => " ".repeat(m.length))
    .replace(/<(script|style)\b[\s\S]*?<\/\1>/gi, (m) => " ".repeat(m.length));
  for (const m of stripped.matchAll(/>([^<]*)</g)) {
    if (/\S/.test(m[1])) found.push(`${at(m.index)}: text outside the catalog: ${m[1].trim().slice(0, 60)}`);
  }
  for (const m of stripped.matchAll(/\s([a-z-]+)\s*=\s*("[^"]*"|'[^']*')/gi)) {
    if (visibleAttributes.has(m[1].toLowerCase()) && /\S/.test(m[2].slice(1, -1))) {
      found.push(`${at(m.index)}: attribute ${m[1]} outside the catalog: ${m[2]}`);
    }
  }
  return found;
}

function lint(file) {
  if (file.endsWith(".html")) return lintHTML(file);
  if (/\.(ts|tsx|mts|cts)$/.test(file) && !file.endsWith(".d.ts") && resolve(file) !== catalog) return lintSource(file);
  return [];
}

const args = process.argv.slice(2);
const expectFail = args[0] === "--expect-fail";
const given = (expectFail ? args.slice(1) : args).map((f) => resolve(f));
const files = given.length ? given : [...walk(join(root, "src")), join(root, "index.html")];

if (expectFail) {
  let bad = 0;
  for (const f of files) {
    const reported = new Set(lint(f).map((x) => Number(x.split(":")[1])));
    const wanted = new Set(readFileSync(f, "utf8").split("\n").flatMap((l, i) => (/(\/\/|\/\*|<!--) want\b/.test(l) ? [i + 1] : [])));
    for (const n of wanted) if (!reported.has(n)) { bad++; console.error(`${relative(root, f)}:${n}: expected a string outside the catalog, found none`); }
    for (const n of reported) if (!wanted.has(n)) { bad++; console.error(`${relative(root, f)}:${n}: reported, but not marked "want"`); }
  }
  process.exit(bad ? 1 : 0);
}

const found = files.flatMap(lint);
for (const f of found) console.error(f);
if (found.length) {
  console.error(`\n${found.length} user-visible string(s) outside the catalog: add them to src/catalog.ts and use t("key").`);
  process.exit(1);
}
