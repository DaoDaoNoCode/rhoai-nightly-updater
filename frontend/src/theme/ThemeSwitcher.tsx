import React from "react";
import {
  Button,
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
        <ToggleGroup isCompact aria-labelledby="theme-color-scheme">
          {(Object.keys(SCHEME_LABELS) as ColorScheme[]).map((s) => (
            <ToggleGroupItem key={s} text={SCHEME_LABELS[s]} buttonId={`theme-scheme-${s}`} isSelected={colorScheme === s} onChange={() => setColorScheme(s)} />
          ))}
        </ToggleGroup>
      </StackItem>
      <StackItem>
        <Title headingLevel="h3" size="md" id="theme-contrast-mode" className="pf-v6-u-mb-sm">Contrast mode</Title>
        <ToggleGroup isCompact aria-labelledby="theme-contrast-mode">
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
    <Popover aria-label="Appearance" bodyContent={body} position="bottom" hasAutoWidth showClose={false}>
      <Button variant="plain" aria-label={`Appearance: ${SCHEME_LABELS[colorScheme]} color scheme, ${CONTRAST_LABELS[contrastMode]} contrast mode`} icon={icon} />
    </Popover>
  );
};
