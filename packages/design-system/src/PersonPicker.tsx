import { useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { Person } from "./Person.tsx";

/** One person to suggest: by email (the value), with their name, photo and a sub line ("Admin"). */
export interface PersonOption {
  email: string;
  name?: string;
  picture?: string;
  /** Under the name: their role, a note. Default: the email. */
  sub?: ReactNode;
  /** Shown greyed and not pickable; the text says why. */
  disabled?: string;
}

export interface PersonPickerProps {
  /** The suggestions: filtered by name or email as the user types. */
  people: PersonOption[];
  /** The email typed or picked. */
  value: string;
  onChange: (email: string) => void;
  /** Any email may be typed (someone not in people). false: only a suggestion counts; the caller checks value against people. Default true. */
  freeText?: boolean;
  label?: ReactNode;
  hint?: ReactNode;
  placeholder?: string;
  autoFocus?: boolean;
  /** At most this many suggestions shown (default 8). */
  limit?: number;
}

const EMAIL_RE = /^[^\s<>@,;"]+@[^\s<>@,;".]+(\.[^\s<>@,;".]+)+$/;

/** A valid email address, as PersonPicker's free text accepts it. */
export function isEmail(s: string): boolean {
  return EMAIL_RE.test(s.trim());
}

/**
 * An email field with suggestions: people (Person rows) whose name or email
 * matches what is typed; a click, or arrows and Enter, picks one. With
 * freeText (the default) any email may be typed: "Set owner" for someone not
 * yet a member. The value is always an email.
 */
export function PersonPicker({ people, value, onChange, freeText = true, label, hint, placeholder = "name@example.com", autoFocus, limit = 8 }: PersonPickerProps) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const q = value.trim().toLowerCase();
  const exact = people.find((p) => p.email.toLowerCase() === q);
  const matches = useMemo(() => (exact ? people : people.filter((p) => !q || p.email.toLowerCase().includes(q) || (p.name ?? "").toLowerCase().includes(q))).slice(0, limit), [people, q, exact, limit]);
  const pick = (p: PersonOption) => {
    if (p.disabled) return;
    onChange(p.email);
    setOpen(false);
  };
  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setOpen(true);
      setActive((i) => Math.min(i + 1, matches.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((i) => Math.max(i - 1, 0));
    } else if (e.key === "Enter" && open && matches[active]) {
      e.preventDefault();
      pick(matches[active]!);
    } else if (e.key === "Escape" && open) {
      // Closes the suggestions, not the dialog around it.
      e.preventDefault();
      e.stopPropagation();
      setOpen(false);
    }
  };
  const invalid = value.trim() !== "" && (freeText ? !isEmail(value) : !exact);
  const shown = open && matches.length > 0;
  return (
    <div className="field person-picker">
      {label && (
        <label className="field-label" htmlFor={id}>
          {label}
        </label>
      )}
      <div className="person-picker-box">
        <input
          ref={input}
          id={id}
          className="input"
          type="text"
          inputMode="email"
          role="combobox"
          aria-expanded={shown}
          aria-controls={`${id}-list`}
          aria-autocomplete="list"
          aria-invalid={invalid || undefined}
          autoComplete="off"
          spellCheck={false}
          autoFocus={autoFocus}
          placeholder={placeholder}
          value={value}
          onChange={(e) => {
            onChange(e.target.value);
            setOpen(true);
            setActive(0);
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => setOpen(false)}
          onKeyDown={onKey}
        />
        {shown && (
          <ul role="listbox" id={`${id}-list`} className="select-pop select-list person-picker-list">
            {matches.map((p, i) => (
              <li
                key={p.email}
                role="option"
                aria-selected={p.email.toLowerCase() === q}
                aria-disabled={p.disabled ? true : undefined}
                title={p.disabled}
                className={["select-opt", i === active ? "is-active" : "", p.disabled ? "is-disabled" : ""].join(" ").trim()}
                // mousedown, not click: the input's blur would close the list first.
                onMouseDown={(e) => {
                  e.preventDefault();
                  pick(p);
                }}
                onMouseEnter={() => setActive(i)}
              >
                <Person name={p.name || p.email} sub={p.sub ?? p.email} picture={p.picture} />
                {p.disabled && <span className="select-opt-why">{p.disabled}</span>}
              </li>
            ))}
          </ul>
        )}
      </div>
      {invalid ? <span className="field-hint field-error">{freeText ? "Not an email address" : "Pick one of the suggestions"}</span> : hint && <span className="field-hint">{hint}</span>}
    </div>
  );
}
