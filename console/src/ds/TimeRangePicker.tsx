import { IconClock } from "./icons.tsx";
import { Select } from "./Select.tsx";

export type TimeRange = "1h" | "6h" | "24h" | "7d" | "30d";

export const TIME_RANGES: { value: TimeRange; label: string; seconds: number }[] = [
  { value: "1h", label: "Last hour", seconds: 3600 },
  { value: "6h", label: "Last 6 hours", seconds: 6 * 3600 },
  { value: "24h", label: "Last 24 hours", seconds: 24 * 3600 },
  { value: "7d", label: "Last 7 days", seconds: 7 * 86400 },
  { value: "30d", label: "Last 30 days", seconds: 30 * 86400 },
];

export function timeRangeSeconds(r: TimeRange): number {
  return TIME_RANGES.find((t) => t.value === r)?.seconds ?? 3600;
}

/** [from, to] in epoch seconds for a range ending now. */
export function timeRangeBounds(r: TimeRange, now = Date.now()): [number, number] {
  const to = Math.floor(now / 1000);
  return [to - timeRangeSeconds(r), to];
}

export interface TimeRangePickerProps {
  value: TimeRange;
  onChange: (r: TimeRange) => void;
  size?: "sm" | "md";
}

export function TimeRangePicker({ value, onChange, size = "sm" }: TimeRangePickerProps) {
  return (
    <Select
      options={TIME_RANGES.map((t) => ({ value: t.value, label: t.label, text: t.value }))}
      value={value}
      onChange={onChange}
      icon={<IconClock size={13} />}
      size={size}
      width={92}
    />
  );
}
