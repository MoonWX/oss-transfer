import type { PopoverOrigin } from "@mui/material";
import { debounce } from "@mui/material/utils";
import { useEffect, useRef, useState } from "react";
import { popupGap } from "../theme";

// Popover keeps menus this far from the viewport edges.
const viewportMargin = 16;
// Below this height a scrolling menu shows too few options to be useful;
// the menu may then cover its anchor instead.
const minimumMenuHeight = 120;

type Horizontal = "left" | "right";

export type MenuPlacement = {
  anchorOrigin: PopoverOrigin;
  transformOrigin: PopoverOrigin;
  maxHeight?: number;
};

// MUI shifts a menu that does not fit below its anchor up over the anchor.
// Open it above the anchor instead when there is more room there, and limit
// its height to the room it has, so the anchor stays visible. gapAbove also
// has to clear a text field's floating label.
export function placeMenu(
  anchor: HTMLElement,
  paper: HTMLElement,
  horizontal: Horizontal = "left",
  gapAbove: number = popupGap.above,
): MenuPlacement {
  const rect = anchor.getBoundingClientRect();
  // scrollHeight ignores a height limit left from an earlier placement.
  const height = paper.scrollHeight + paper.offsetHeight - paper.clientHeight;
  const roomBelow =
    window.innerHeight - rect.bottom - popupGap.below - viewportMargin;
  const roomAbove = rect.top - gapAbove - viewportMargin;
  const above = height > roomBelow && roomAbove > roomBelow;
  const room = Math.floor(above ? roomAbove : roomBelow);
  const maxHeight =
    height > room && room >= minimumMenuHeight ? room : undefined;
  return above
    ? {
        anchorOrigin: { vertical: -gapAbove, horizontal },
        transformOrigin: { vertical: "bottom", horizontal },
        maxHeight,
      }
    : {
        anchorOrigin: {
          vertical: rect.height + popupGap.below,
          horizontal,
        },
        transformOrigin: { vertical: "top", horizontal },
        maxHeight,
      };
}

// The CSS height limit for a placement. Menus already stop 96px short of the
// window; keeping that limit too keeps a menu on screen when the window
// shrinks while it is open.
export const menuMaxHeight = (placement: MenuPlacement) =>
  placement.maxHeight === undefined
    ? undefined
    : `min(${placement.maxHeight}px, calc(100% - 96px))`;

// MUI focuses the selected (or first) item, which scrolls it into view,
// before a new height limit applies. Scroll it into the limited menu again.
export function revealFocusedItem(paper: HTMLElement) {
  const active = paper.ownerDocument.activeElement;
  const item =
    active instanceof HTMLElement && active !== paper && paper.contains(active)
      ? active
      : paper.querySelector<HTMLElement>('[aria-selected="true"]');
  if (!item) return;
  // Offsets ignore the scale of the opening transition.
  let top = 0;
  let node: HTMLElement | null = item;
  while (node && node !== paper) {
    top += node.offsetTop;
    node = node.offsetParent as HTMLElement | null;
  }
  if (node !== paper) return;
  const bottom = top + item.offsetHeight;
  if (top < paper.scrollTop) paper.scrollTop = top;
  else if (bottom > paper.scrollTop + paper.clientHeight)
    paper.scrollTop = bottom - paper.clientHeight;
}

/**
 * Places a menu below its anchor, or above it when only the space above
 * fits, re-placing it when the window size changes while it is open. Pass
 * onEnter and onExited to the menu's transition, the origins to the menu
 * and maxHeight to its paper style.
 */
export function useMenuPlacement(
  horizontal: Horizontal = "left",
  gapAbove: number = popupGap.above,
) {
  const [placement, setPlacement] = useState<MenuPlacement>(() => ({
    anchorOrigin: { vertical: "bottom", horizontal },
    transformOrigin: { vertical: "top", horizontal },
  }));
  const [open, setOpen] = useState(false);
  const target = useRef<{ anchor: HTMLElement; paper: HTMLElement } | null>(
    null,
  );
  useEffect(() => {
    if (!open) return undefined;
    // Rotating a phone or resizing the window moves the anchor.
    const update = debounce(() => {
      const current = target.current;
      if (current)
        setPlacement(
          placeMenu(current.anchor, current.paper, horizontal, gapAbove),
        );
    });
    window.addEventListener("resize", update);
    return () => {
      update.clear();
      window.removeEventListener("resize", update);
    };
  }, [open, horizontal, gapAbove]);
  return {
    anchorOrigin: placement.anchorOrigin,
    transformOrigin: placement.transformOrigin,
    maxHeight: menuMaxHeight(placement),
    // The menu is rendered but still invisible here, so its real height
    // decides where it opens.
    onEnter: (anchor: HTMLElement | null | undefined, paper: HTMLElement) => {
      if (!anchor) return;
      const next = placeMenu(anchor, paper, horizontal, gapAbove);
      paper.style.maxHeight = menuMaxHeight(next) ?? "";
      revealFocusedItem(paper);
      target.current = { anchor, paper };
      setPlacement(next);
      setOpen(true);
    },
    onExited: () => {
      target.current = null;
      setOpen(false);
    },
  };
}
