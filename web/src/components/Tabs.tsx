import {
  Fragment,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";

export type TabItem<T extends string = string> = {
  id: T;
  label: ReactNode;
  /** Small trailing count, e.g. pending items. */
  count?: number;
  icon?: ReactNode;
  disabled?: boolean;
  /** A group title rendered before this tab (vertical bars only). */
  heading?: string;
};

type Props<T extends string> = {
  items: TabItem<T>[];
  value: T;
  onChange: (id: T) => void;
  label: string;
  /** `pill` slides a filled pill, `underline` a thin bar, `segment` a raised thumb. */
  variant?: "pill" | "underline" | "segment";
  orientation?: "horizontal" | "vertical";
  className?: string;
};

const reduced = () =>
  typeof matchMedia === "function" && matchMedia("(prefers-reduced-motion: reduce)").matches;

/**
 * One tab bar for the whole console. The active marker is a single element that
 * slides between tabs, so every menu moves the same way. Content that follows a
 * tab bar should be wrapped in <TabPanel> so it arrives from the matching side.
 */
export function TabBar<T extends string>({
  items,
  value,
  onChange,
  label,
  variant = "pill",
  orientation = "horizontal",
  className = "",
}: Props<T>) {
  const list = useRef<HTMLDivElement>(null);
  const [box, setBox] = useState<{ pos: number; size: number } | null>(null);
  const [ready, setReady] = useState(false);
  const vertical = orientation === "vertical";

  const measure = useCallback(() => {
    const root = list.current;
    const active = root?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]');
    if (!root || !active) return;
    setBox(
      vertical
        ? { pos: active.offsetTop, size: active.offsetHeight }
        : { pos: active.offsetLeft, size: active.offsetWidth },
    );
    if (!vertical) {
      // Keep the active tab visible when the bar scrolls on narrow screens.
      const left = active.offsetLeft;
      const right = left + active.offsetWidth;
      if (left < root.scrollLeft + 8) root.scrollTo({ left: Math.max(0, left - 16), behavior: reduced() ? "auto" : "smooth" });
      else if (right > root.scrollLeft + root.clientWidth - 8) root.scrollTo({ left: right - root.clientWidth + 16, behavior: reduced() ? "auto" : "smooth" });
    }
  }, [vertical]);

  useLayoutEffect(measure, [measure, value, items.length]);
  useEffect(() => {
    const root = list.current;
    if (!root) return;
    const observer = new ResizeObserver(measure);
    observer.observe(root);
    void document.fonts?.ready.then(measure);
    // Enable the slide only after the first position is painted.
    const frame = requestAnimationFrame(() => requestAnimationFrame(() => setReady(true)));
    return () => {
      observer.disconnect();
      cancelAnimationFrame(frame);
    };
  }, [measure]);

  function key(e: KeyboardEvent<HTMLDivElement>) {
    const enabled = items.filter((i) => !i.disabled);
    const at = enabled.findIndex((i) => i.id === value);
    const next = vertical ? ["ArrowDown", "ArrowUp"] : ["ArrowRight", "ArrowLeft"];
    let target = -1;
    if (e.key === next[0]) target = (at + 1) % enabled.length;
    else if (e.key === next[1]) target = (at - 1 + enabled.length) % enabled.length;
    else if (e.key === "Home") target = 0;
    else if (e.key === "End") target = enabled.length - 1;
    if (target < 0) return;
    e.preventDefault();
    onChange(enabled[target].id);
    requestAnimationFrame(() =>
      list.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')?.focus(),
    );
  }

  return (
    <div
      ref={list}
      role="tablist"
      aria-label={label}
      aria-orientation={orientation}
      className={`tabbar ${className}`}
      data-variant={variant}
      data-orientation={orientation}
      data-ready={ready}
      onKeyDown={key}
    >
      {box && (
        <span
          className="tabbar-indicator"
          aria-hidden="true"
          style={
            vertical
              ? { transform: `translateY(${box.pos}px)`, height: box.size }
              : { transform: `translateX(${box.pos}px)`, width: box.size }
          }
        />
      )}
      {items.map((item) => (
        <Fragment key={item.id}>
        {item.heading && vertical && <span role="presentation" className="tabbar-heading">{item.heading}</span>}
        <button
          type="button"
          role="tab"
          id={`tab-${item.id}`}
          aria-selected={item.id === value}
          tabIndex={item.id === value ? 0 : -1}
          disabled={item.disabled}
          onClick={() => onChange(item.id)}
        >
          {item.icon}
          <span>{item.label}</span>
          {item.count != null && item.count > 0 && <small className="tabbar-count">{item.count}</small>}
        </button>
        </Fragment>
      ))}
    </div>
  );
}

/**
 * Wraps the content that belongs to one tab. When the tab changes the new
 * content arrives from the side the tab moved toward, and its first blocks
 * settle in a short sequence. Content stays visible without animation support.
 */
export function TabPanel({ value, order, children }: { value: string; order: string[]; children: ReactNode }) {
  const previous = useRef(order.indexOf(value));
  const index = order.indexOf(value);
  const direction = index === previous.current ? "none" : index > previous.current ? "forward" : "back";
  useEffect(() => {
    previous.current = index;
  }, [index]);
  return (
    <div key={value} role="tabpanel" aria-labelledby={`tab-${value}`} className="tab-panel" data-direction={direction}>
      {children}
    </div>
  );
}
