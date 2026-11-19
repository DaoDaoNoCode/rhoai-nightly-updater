import React, { useRef, useState } from "react";
import { MenuToggle, onToggleArrowKeydownDefault, Select, SelectList, SelectOption } from "@patternfly/react-core";

/**
 * A short fixed list to pick one value from: PatternFly's single Select
 * (MenuToggle + SelectList) instead of a native select, whose open list is
 * the browser's. The toggle is named by label and described by the value it
 * shows, as a native select reads both; the selected option is marked.
 * Choosing an option closes the menu and calls onSelect with its value,
 * even the current one. Escape closes the menu only (not a dialog around
 * it) and puts the focus back on the toggle.
 */
export const SingleSelect: React.FC<{
  id: string;
  /** The toggle's accessible name ("Set ray"). */
  label: string;
  /** The selected value, if any (none: nothing is marked). */
  selected: string | undefined;
  /** What the toggle shows. */
  text: string;
  options: { value: string; label: string }[];
  onSelect: (value: string) => void;
  isDisabled?: boolean;
  /** The toggle shows text as a placeholder: the field has no value. */
  isPlaceholder?: boolean;
  /** The value is wrong: PatternFly's danger status and aria-invalid. */
  invalid?: boolean;
  /** The menu stays in the DOM next to the toggle: needed inside a Modal (its focus trap). Otherwise it goes to document.body, over any scroll container. */
  inline?: boolean;
}> = ({ id, label, selected, text, options, onSelect, isDisabled = false, isPlaceholder, invalid = false, inline = false }) => {
  const [isOpen, setIsOpen] = useState(false);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const valueId = `${id}-value`;
  const open = isOpen && !isDisabled;
  // Both handlers run in the capture phase, before listeners further up that
  // would take the key first: a Modal closes on Escape from a keydown
  // listener on document.body, and a tree table (ComponentTree) moves the
  // focus to the next row on an arrow key from a document listener, which
  // stops the event before Select's own window listener sees it.
  const closeOnEscape = (e: React.KeyboardEvent) => {
    if (!open || e.key !== "Escape") return false;
    e.stopPropagation();
    e.preventDefault();
    setIsOpen(false);
    toggleRef.current?.focus();
    return true;
  };
  const onToggleKeyDownCapture = (e: React.KeyboardEvent) => {
    if (closeOnEscape(e) || !open || (e.key !== "ArrowDown" && e.key !== "ArrowUp")) return;
    // Open: down moves to the first option, up to the last, as Select does.
    e.stopPropagation();
    onToggleArrowKeydownDefault(e.nativeEvent, menuRef);
  };
  const toggle = (
    <MenuToggle ref={toggleRef} id={id} aria-label={label} aria-describedby={valueId} aria-haspopup="listbox" aria-invalid={invalid || undefined}
      status={invalid ? "danger" : undefined} isExpanded={open} isDisabled={isDisabled} isFullWidth isPlaceholder={isPlaceholder}
      onClick={() => setIsOpen(!isOpen)} onKeyDownCapture={onToggleKeyDownCapture}>
      <span id={valueId}>{text}</span>
    </MenuToggle>
  );
  return (
    <Select ref={menuRef} isOpen={open} selected={selected} toggle={{ toggleNode: toggle, toggleRef }} shouldFocusToggleOnSelect onKeyDownCapture={closeOnEscape}
      popperProps={{ width: "trigger", ...(inline ? { appendTo: "inline" as const } : {}) }}
      onOpenChange={setIsOpen} onSelect={(_e, v) => { setIsOpen(false); if (typeof v === "string") onSelect(v); }}>
      <SelectList aria-label={label}>
        {options.map((o) => <SelectOption key={o.value} value={o.value}>{o.label}</SelectOption>)}
      </SelectList>
    </Select>
  );
};
