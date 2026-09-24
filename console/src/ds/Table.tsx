import { useMemo, useState, type ReactNode } from "react";
import { EmptyState } from "./EmptyState.tsx";
import { Skeleton } from "./Spinner.tsx";
import { IconChevronDown, IconChevronUp } from "./icons.tsx";

export interface Column<Row> {
  key: string;
  header: ReactNode;
  /** Cell renderer. */
  cell: (row: Row) => ReactNode;
  /** Value used for sorting; omit to make the column unsortable. */
  sortValue?: (row: Row) => string | number | null | undefined;
  align?: "left" | "right";
  /** Monospace + tabular numerals (ids, hosts, numbers). */
  mono?: boolean;
  width?: number | string;
  /** Prevent wrapping and let the cell ellipsize. */
  nowrap?: boolean;
}

export interface SortState {
  key: string;
  dir: "asc" | "desc";
}

export interface TableProps<Row> {
  columns: Column<Row>[];
  rows: Row[];
  rowKey: (row: Row) => string;
  onRowClick?: (row: Row) => void;
  /** Currently selected row key (highlighted). */
  selected?: string | null;
  sort?: SortState;
  onSortChange?: (s: SortState) => void;
  /** Uncontrolled default sort. */
  defaultSort?: SortState;
  loading?: boolean;
  /** Rows to render as skeletons while loading with no data. */
  loadingRows?: number;
  empty?: ReactNode;
  /** Height for the scroll container; header sticks inside it. */
  maxHeight?: number | string;
  /** Extra-dense rows (28px). */
  dense?: boolean;
  className?: string;
}

export function Table<Row>(props: TableProps<Row>) {
  const { columns, rows, rowKey, onRowClick, selected, loading, loadingRows = 6, empty, maxHeight, dense, className } = props;
  const [internalSort, setInternalSort] = useState<SortState | undefined>(props.defaultSort);
  const sort = props.sort ?? internalSort;
  const setSort = props.onSortChange ?? setInternalSort;

  const sorted = useMemo(() => {
    if (!sort) return rows;
    const col = columns.find((c) => c.key === sort.key);
    if (!col?.sortValue) return rows;
    const sv = col.sortValue;
    const dir = sort.dir === "asc" ? 1 : -1;
    return [...rows].sort((a, b) => {
      const x = sv(a);
      const y = sv(b);
      if (x == null && y == null) return 0;
      if (x == null) return 1;
      if (y == null) return -1;
      if (typeof x === "number" && typeof y === "number") return (x - y) * dir;
      return String(x).localeCompare(String(y)) * dir;
    });
  }, [rows, sort, columns]);

  const toggleSort = (c: Column<Row>) => {
    if (!c.sortValue) return;
    if (sort?.key === c.key) setSort({ key: c.key, dir: sort.dir === "asc" ? "desc" : "asc" });
    else setSort({ key: c.key, dir: c.align === "right" ? "desc" : "asc" });
  };

  const showSkeleton = loading && rows.length === 0;
  const showEmpty = !loading && rows.length === 0;

  return (
    <div className={["table-wrap", loading && rows.length > 0 ? "is-refreshing" : "", className ?? ""].join(" ").trim()} style={maxHeight ? { maxHeight } : undefined}>
      <table className={dense ? "table table-dense" : "table"}>
        <thead>
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  style={{ width: c.width, textAlign: c.align ?? "left" }}
                  className={[c.sortValue ? "is-sortable" : "", active ? "is-sorted" : ""].join(" ").trim()}
                  aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : undefined}
                  onClick={() => toggleSort(c)}
                >
                  <span className="th-inner">
                    {c.header}
                    {c.sortValue && (
                      <span className="th-sort" aria-hidden="true">
                        {active ? sort.dir === "asc" ? <IconChevronUp size={11} /> : <IconChevronDown size={11} /> : <IconChevronDown size={11} className="th-sort-idle" />}
                      </span>
                    )}
                  </span>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {showSkeleton &&
            Array.from({ length: loadingRows }, (_, i) => (
              <tr key={`sk-${i}`} className="row-skeleton">
                {columns.map((c) => (
                  <td key={c.key} style={{ textAlign: c.align ?? "left" }}>
                    <Skeleton width={`${50 + ((i * 7 + c.key.length * 13) % 45)}%`} />
                  </td>
                ))}
              </tr>
            ))}
          {showEmpty && (
            <tr>
              <td colSpan={columns.length} className="td-empty">
                {typeof empty === "string" || empty == null ? <EmptyState compact title={empty ?? "Nothing here"} /> : empty}
              </td>
            </tr>
          )}
          {sorted.map((row) => {
            const k = rowKey(row);
            return (
              <tr
                key={k}
                className={[onRowClick ? "is-clickable" : "", selected === k ? "is-selected" : ""].join(" ").trim()}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                tabIndex={onRowClick ? 0 : undefined}
                onKeyDown={
                  onRowClick
                    ? (e) => {
                        if (e.key === "Enter") onRowClick(row);
                      }
                    : undefined
                }
              >
                {columns.map((c) => (
                  <td
                    key={c.key}
                    style={{ textAlign: c.align ?? "left" }}
                    className={[c.mono ? "mono" : "", c.nowrap ? "td-nowrap" : "", c.align === "right" ? "num" : ""].join(" ").trim() || undefined}
                  >
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
