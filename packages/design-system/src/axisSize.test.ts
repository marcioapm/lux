import { expect, test } from "bun:test";
import { AXIS_MIN_SIZE, axisSize, measuredAxisSize } from "./axisSize.ts";

// About 6.5px per character, close to 11px Inter's digits.
const measure = (text: string) => text.length * 6.5;

test("axisSize is the widest label rounded up, plus gap and inset", () => {
  // "186.3 GiB": 9 * 6.5 = 58.5 → 59 + 5 + 4.
  expect(axisSize(["0 B", "100 GiB", "186.3 GiB", "200 GiB"], measure)).toBe(68);
  // "$12,345.50": 10 * 6.5 = 65 + 5 + 4.
  expect(axisSize(["453.7 MiB", "$12,345.50"], measure)).toBe(74);
  expect(axisSize(["fractional"], () => 47.1)).toBe(57);
  expect(axisSize(["fractional"], () => 47.1, 80)).toBe(80);
});

test("axisSize keeps the 56px floor for short labels", () => {
  expect(axisSize(["0", "12", "40%", "100%"], measure)).toBe(AXIS_MIN_SIZE);
  expect(axisSize([], measure)).toBe(AXIS_MIN_SIZE);
  expect(axisSize([null, undefined, ""], measure)).toBe(AXIS_MIN_SIZE);
});

test("axisSize is stable for the same labels, so uPlot's resize cycle converges", () => {
  const labels = ["0 B", "250 GiB", "500 GiB"];
  const first = axisSize(labels, measure);
  expect(axisSize([...labels], measure)).toBe(first);
  expect(axisSize(labels, measure)).toBe(axisSize(labels.slice().reverse(), measure));
  // Without a canvas (no DOM) measuredAxisSize falls back to the floor and repeats it.
  const size = measuredAxisSize("11px sans-serif");
  expect([size(null, null), size(null, labels), size(null, labels)]).toEqual([AXIS_MIN_SIZE, AXIS_MIN_SIZE, AXIS_MIN_SIZE]);
});
