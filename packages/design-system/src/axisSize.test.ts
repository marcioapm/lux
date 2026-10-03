import { expect, test } from "bun:test";
import { AXIS_MIN_SIZE, axisSize, measuredAxisSize } from "./axisSize.ts";

// About 6.5px per character, close to 11px Inter's digits.
const measure = (text: string) => text.length * 6.5;

test("axisSize widens the axis to fit long labels", () => {
  const labels = ["0 B", "100 GiB", "186.3 GiB", "200 GiB"];
  const size = axisSize(labels, measure);
  expect(size).toBeGreaterThan(AXIS_MIN_SIZE);
  expect(size).toBeGreaterThanOrEqual(Math.max(...labels.map(measure)) + 5);
  expect(axisSize(["453.7 MiB", "$12,345.50"], measure)).toBeGreaterThanOrEqual(measure("$12,345.50") + 5);
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
