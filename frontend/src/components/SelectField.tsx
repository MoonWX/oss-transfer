import {
  TextField,
  type MenuProps,
  type SelectProps,
  type TextFieldProps,
} from "@mui/material";
import { useRef, type CSSProperties } from "react";
import { useMenuPlacement } from "../lib/menuPlacement";

type TransitionCallbacks = {
  onEnter?: (node: HTMLElement, isAppearing: boolean) => void;
  onExited?: (node: HTMLElement) => void;
};

/** A select text field whose menu never covers the field or its label. */
export default function SelectField({
  slotProps,
  ...props
}: Omit<TextFieldProps, "select">) {
  const field = useRef<HTMLElement | null>(null);
  const placement = useMenuPlacement();
  const select = slotProps?.select;
  if (typeof select === "function")
    return <TextField {...props} select slotProps={slotProps} />;
  const selectProps = (select ?? {}) as Partial<SelectProps>;
  const menu: Partial<MenuProps> = selectProps.MenuProps ?? {};
  const paper = (menu.slotProps?.paper ?? {}) as { style?: CSSProperties };
  const transition = (menu.slotProps?.transition ?? {}) as TransitionCallbacks;
  return (
    <TextField
      {...props}
      select
      slotProps={{
        ...slotProps,
        select: {
          ...selectProps,
          onOpen: (event) => {
            // The menu is anchored to the field's outlined box.
            field.current = (event.currentTarget as HTMLElement).parentElement;
            selectProps.onOpen?.(event);
          },
          MenuProps: {
            ...menu,
            anchorOrigin: placement.anchorOrigin,
            transformOrigin: placement.transformOrigin,
            slotProps: {
              ...menu.slotProps,
              paper: {
                ...paper,
                style: { ...paper.style, maxHeight: placement.maxHeight },
              },
              transition: {
                ...transition,
                onEnter: (node: HTMLElement, isAppearing: boolean) => {
                  placement.onEnter(field.current, node);
                  transition.onEnter?.(node, isAppearing);
                },
                onExited: (node: HTMLElement) => {
                  placement.onExited();
                  transition.onExited?.(node);
                },
              },
            },
          },
        },
      }}
    />
  );
}
