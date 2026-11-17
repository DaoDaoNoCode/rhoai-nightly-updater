import React, { useEffect, useRef, useState } from "react";
import {
  Button,
  MenuToggle,
  type MenuToggleElement,
  Select,
  SelectList,
  SelectOption,
  TextInputGroup,
  TextInputGroupMain,
  TextInputGroupUtilities,
} from "@patternfly/react-core";
import TimesIcon from "@patternfly/react-icons/dist/esm/icons/times-icon";

export interface ProjectOption {
  name: string;
  /** What this page has set up there, for example "Pipeline server: Ready". */
  description?: string;
}

/**
 * A data science project picker: PatternFly's typeahead Select (type to
 * filter, arrow keys to move, Enter to choose, Escape to close).
 */
export const ProjectSelect: React.FC<{
  id: string;
  projects: ProjectOption[];
  selected: string | null;
  onSelect: (project: string) => void;
  /** The clear button clears the selection too, not only the typed filter. */
  onClear?: () => void;
}> = ({ id, projects, selected, onSelect, onClear }) => {
  const [isOpen, setIsOpen] = useState(false);
  const [inputValue, setInputValue] = useState(selected ?? "");
  const [filter, setFilter] = useState("");
  const [focused, setFocused] = useState<number | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = `${id}-listbox`;

  useEffect(() => { setInputValue(selected ?? ""); }, [selected]);

  const shown = filter ? projects.filter((p) => p.name.toLowerCase().includes(filter.toLowerCase())) : projects;
  const optionId = (name: string) => `${id}-option-${name}`;
  const activeId = focused !== null && shown[focused] ? optionId(shown[focused].name) : undefined;

  const close = () => {
    setIsOpen(false);
    setFocused(null);
    setFilter("");
    setInputValue(selected ?? "");
  };
  const choose = (name: string) => {
    onSelect(name);
    setInputValue(name);
    setFilter("");
    setIsOpen(false);
    setFocused(null);
  };
  const onKeyDown = (e: React.KeyboardEvent) => {
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp": {
        e.preventDefault();
        if (!isOpen) setIsOpen(true);
        if (shown.length === 0) return;
        const step = e.key === "ArrowDown" ? 1 : -1;
        setFocused((f) => (f === null ? (step > 0 ? 0 : shown.length - 1) : (f + step + shown.length) % shown.length));
        return;
      }
      case "Enter":
        e.preventDefault();
        if (isOpen && focused !== null && shown[focused]) choose(shown[focused].name);
        else if (!isOpen) setIsOpen(true);
        return;
      case "Escape":
        if (isOpen) {
          e.stopPropagation();
          close();
        }
        return;
    }
  };

  const toggle = (ref: React.Ref<MenuToggleElement>) => (
    <MenuToggle ref={ref} variant="typeahead" isExpanded={isOpen} isFullWidth aria-label="Show projects"
      onClick={() => { setIsOpen(!isOpen); inputRef.current?.focus(); }}>
      <TextInputGroup isPlain>
        <TextInputGroupMain
          id={id}
          value={inputValue}
          placeholder="Select a project"
          role="combobox"
          isExpanded={isOpen}
          aria-controls={listId}
          aria-activedescendant={activeId}
          autoComplete="off"
          innerRef={inputRef}
          onClick={() => setIsOpen(!isOpen)}
          onChange={(_e, v) => {
            setInputValue(v);
            setFilter(v);
            setFocused(null);
            if (!isOpen) setIsOpen(true);
          }}
          onKeyDown={onKeyDown}
        />
        {inputValue && (
          <TextInputGroupUtilities>
            <Button variant="plain" aria-label="Clear the project filter" icon={<TimesIcon />}
              onClick={() => { setInputValue(""); setFilter(""); setFocused(null); if (selected) onClear?.(); inputRef.current?.focus(); }} />
          </TextInputGroupUtilities>
        )}
      </TextInputGroup>
    </MenuToggle>
  );

  return (
    <Select id={`${id}-select`} isOpen={isOpen} selected={selected ?? undefined} onOpenChange={(open) => { if (!open) close(); }}
      onSelect={(_e, v) => { if (typeof v === "string" && v) choose(v); }} toggle={toggle} variant="typeahead">
      <SelectList id={listId} aria-label="Data science projects">
        {shown.map((p, i) => (
          <SelectOption key={p.name} id={optionId(p.name)} value={p.name} description={p.description} isFocused={focused === i}>{p.name}</SelectOption>
        ))}
        {shown.length === 0 && <SelectOption isDisabled value="">{projects.length === 0 ? "No data science projects" : `No project matches "${filter}"`}</SelectOption>}
      </SelectList>
    </Select>
  );
};
