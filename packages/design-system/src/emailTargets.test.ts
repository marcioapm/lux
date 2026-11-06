import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

// EmailInput's small controls (a chip's ✕, Remove left out, Copy their
// link) each get a hit area of at least 24×24 without growing the 24px chip.
// The browser measurement is the console's screenshot step; here, that the
// rules exist and say so.
const css = readFileSync(new URL("./components.css", import.meta.url), "utf8");
const src = readFileSync(new URL("./EmailInput.tsx", import.meta.url), "utf8");

/** The declarations of the first rule whose selector list is exactly sel. */
function rule(sel: string): Record<string, string> {
  const esc = sel.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = new RegExp(`(?:^|\\n)${esc}\\s*\\{([^}]*)\\}`).exec(css);
  if (!m) throw new Error(`no rule ${sel}`);
  return Object.fromEntries(
    m[1]!
      .split(";")
      .map((d) => d.replace(/\/\*[\s\S]*?\*\//g, "").trim())
      .filter(Boolean)
      .map((d) => [d.slice(0, d.indexOf(":")).trim(), d.slice(d.indexOf(":") + 1).trim()]),
  );
}
const px = (v: string | undefined) => (v == null ? NaN : parseFloat(v));

test("chip remove: a 20px glyph with a ::before hit box of 24×24, inside the 24px chip", () => {
  const chip = rule(".emailchip");
  const x = rule(".emailchip-remove");
  const hit = rule(".emailchip-remove::before");
  expect(px(chip.height)).toBe(24);
  expect(x.position).toBe("relative");
  expect(hit.position).toBe("absolute");
  expect(hit.content).toBe('""');
  const grow = -px(hit.inset);
  expect(px(x.width) + 2 * grow).toBeGreaterThanOrEqual(24);
  expect(px(x.height) + 2 * grow).toBeGreaterThanOrEqual(24);
  // Within the chip's height: the hit box is not taller than the chip.
  expect(px(x.height) + 2 * grow).toBeLessThanOrEqual(px(chip.height));
  expect(src).toContain('className="emailchip-remove"');
});

test("Remove left out and Copy their link: a ::before hit box at least 24px each way", () => {
  const b = rule(".emailinput-out .linkbtn");
  const hit = rule(".emailinput-out .linkbtn::before");
  expect(b.position).toBe("relative");
  expect(hit.position).toBe("absolute");
  expect(hit.width).toBe("max(100%, 24px)");
  expect(hit.height).toBe("max(100%, 24px)");
  // Both actions are .linkbtn inside .emailinput-out.
  const out = src.slice(src.indexOf('className="emailinput-out"'));
  expect(out.match(/className="linkbtn"/g)?.length).toBeGreaterThanOrEqual(2);
});
