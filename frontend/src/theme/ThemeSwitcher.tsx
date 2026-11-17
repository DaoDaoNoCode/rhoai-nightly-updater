import React from "react";
import {
  Button,
  Divider,
  DropdownGroup,
  DropdownItem,
  DropdownList,
  HelperText,
  HelperTextItem,
  Popover,
  Stack,
  StackItem,
  Title,
  ToggleGroup,
  ToggleGroupItem,
} from "@patternfly/react-core";
import RhUiAutoLightDarkModeIcon from "@patternfly/react-icons/dist/esm/icons/rh-ui-auto-light-dark-mode-icon";
import RhUiDarkModeIcon from "@patternfly/react-icons/dist/esm/icons/rh-ui-dark-mode-icon";
import RhUiLightModeIcon from "@patternfly/react-icons/dist/esm/icons/rh-ui-light-mode-icon";
import { useTheme } from "./ThemeContext";
import type { ColorScheme, ContrastMode, EffectiveContrast } from "./themeCore";

const SCHEME_LABELS: Record<ColorScheme, string> = { system: "System", light: "Light", dark: "Dark" };
const CONTRAST_LABELS: Record<ContrastMode, string> = { system: "System", default: "Default", "high-contrast": "High contrast", glass: "Glass" };
const EFFECTIVE_LABELS: Record<EffectiveContrast, string> = { default: "default contrast", "high-contrast": "high contrast", glass: "glass" };

/**
 * The masthead's appearance control: Color scheme (System, Light, Dark) and
 * Contrast mode (System, Default, High contrast, Glass) as two toggle groups
 * in a popover. System follows the operating system; the line below says
 * what it resolves to now.
 */
export const ThemeSwitcher: React.FC = () => {
  const { colorScheme, contrastMode, resolved, setColorScheme, setContrastMode } = useTheme();
  const icon = colorScheme === "system" ? <RhUiAutoLightDarkModeIcon /> : colorScheme === "dark" ? <RhUiDarkModeIcon /> : <RhUiLightModeIcon />;
  const followsSystem = colorScheme === "system" || contrastMode === "system";
  const now = `${resolved.dark ? "Dark" : "Light"}, ${EFFECTIVE_LABELS[resolved.contrast]}`;

  const body = (
    <Stack hasGutter>
      <StackItem>
        <Title headingLevel="h3" size="md" id="theme-color-scheme" className="pf-v6-u-mb-sm">Color scheme</Title>
        <ToggleGroup aria-labelledby="theme-color-scheme">
          {(Object.keys(SCHEME_LABELS) as ColorScheme[]).map((s) => (
            <ToggleGroupItem key={s} text={SCHEME_LABELS[s]} buttonId={`theme-scheme-${s}`} isSelected={colorScheme === s} onChange={() => setColorScheme(s)} />
          ))}
        </ToggleGroup>
      </StackItem>
      <StackItem>
        <Title headingLevel="h3" size="md" id="theme-contrast-mode" className="pf-v6-u-mb-sm">Contrast mode</Title>
        <ToggleGroup aria-labelledby="theme-contrast-mode">
          {(Object.keys(CONTRAST_LABELS) as ContrastMode[]).map((m) => (
            <ToggleGroupItem key={m} text={CONTRAST_LABELS[m]} buttonId={`theme-contrast-${m}`} isSelected={contrastMode === m} onChange={() => setContrastMode(m)} />
          ))}
        </ToggleGroup>
      </StackItem>
      {followsSystem && (
        <StackItem>
          <HelperText>
            <HelperTextItem>Following the system: {now}.</HelperTextItem>
          </HelperText>
        </StackItem>
      )}
    </Stack>
  );

  return (
    <Popover aria-label="Appearance" bodyContent={body} position="bottom" hasAutoWidth>
      <Button variant="plain" aria-label={`Appearance: ${SCHEME_LABELS[colorScheme]} color scheme, ${CONTRAST_LABELS[contrastMode]} contrast mode`} icon={icon} />
    </Popover>
  );
};

/**
 * The same two choices as menu groups, for the masthead's overflow (user)
 * menu on screens too narrow for the popover, where the masthead hides the
 * ThemeSwitcher (PatternFly's masthead pattern: toolbar items move into the
 * overflow menu). The chosen item of each group is checked; choosing one
 * applies it at once. className goes on the groups and the divider between
 * them, so a menu that is shown at every width can hide them where the
 * popover shows. The host menu separates them from its other groups.
 */
export const ThemeMenuGroups: React.FC<{ className?: string }> = ({ className }) => {
  const { colorScheme, contrastMode, setColorScheme, setContrastMode } = useTheme();
  // PatternFly 6.7 marks the chosen item with a check mark only: MenuItem
  // sets role "menuitem" itself (aria-selected only when the whole menu is
  // a listbox, Menu/MenuItem.js:219), puts extra props on the <li
  // role="none"> (:235), and has no menuitemradio or aria-checked. This
  // menu also holds links and Log out, so it cannot be a listbox; the
  // accessible name says which item is chosen and keeps the visible text.
  const name = (label: string, selected: boolean) => (selected ? `${label}, selected` : label);
  return (
    <>
      <DropdownGroup className={className} label="Color scheme" labelHeadingLevel="h2">
        <DropdownList>
          {(Object.keys(SCHEME_LABELS) as ColorScheme[]).map((s) => (
            <DropdownItem key={s} value={`scheme-${s}`} isSelected={colorScheme === s} aria-label={name(SCHEME_LABELS[s], colorScheme === s)} onClick={() => setColorScheme(s)}>
              {SCHEME_LABELS[s]}
            </DropdownItem>
          ))}
        </DropdownList>
      </DropdownGroup>
      <Divider className={className} />
      <DropdownGroup className={className} label="Contrast mode" labelHeadingLevel="h2">
        <DropdownList>
          {(Object.keys(CONTRAST_LABELS) as ContrastMode[]).map((m) => (
            <DropdownItem key={m} value={`contrast-${m}`} isSelected={contrastMode === m} aria-label={name(CONTRAST_LABELS[m], contrastMode === m)} onClick={() => setContrastMode(m)}>
              {CONTRAST_LABELS[m]}
            </DropdownItem>
          ))}
        </DropdownList>
      </DropdownGroup>
    </>
  );
};
