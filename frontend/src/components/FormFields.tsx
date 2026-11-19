import React from "react";
import { FormHelperText, HelperText, HelperTextItem, type HelperTextItemProps } from "@patternfly/react-core";
import helperTextMarginTop from "@patternfly/react-tokens/dist/esm/c_form__helper_text_MarginBlockStart";
import helperTextMarginTopBase from "@patternfly/react-tokens/dist/esm/c_form__helper_text_MarginBlockStart_base";

/**
 * A field's helper or error text, placed as PatternFly places it: in
 * FormHelperText, which keeps spacer-xs between the control and the text.
 * A bare HelperText has no margin of its own and touches the control.
 */
export const FieldHelperText: React.FC<{
  id?: string;
  variant?: HelperTextItemProps["variant"];
  children: React.ReactNode;
}> = ({ id, variant = "default", children }) => (
  <FormHelperText>
    <HelperText id={id}>
      <HelperTextItem variant={variant}>{children}</HelperTextItem>
    </HelperText>
  </FormHelperText>
);

/**
 * PatternFly's own helper-text spacing, for fields inside a radio's or a
 * checkbox's body. A stacked FormGroup (isStack, as every radio group is)
 * sets --pf-v6-c-form__helper-text--MarginBlockStart to minus its row gap
 * plus spacer-xs, for helper text that is one of its own rows. Custom
 * properties inherit, so helper text deeper inside (a field in a radio's
 * body) got the same negative margin and overlapped its control by 4px,
 * 8px closer than in any standalone field. This sets the variable back to
 * PatternFly's base value for everything inside.
 */
export const nestedFieldsStyle = { [helperTextMarginTop.name]: helperTextMarginTopBase.var } as React.CSSProperties;

/** Fields inside a radio's or checkbox's body: see nestedFieldsStyle. */
export const NestedFields: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <div style={nestedFieldsStyle}>{children}</div>
);
