import { useEffect, useRef } from "react";
import type { ButtonHTMLAttributes, ComponentProps, ReactNode } from "react";

// 几个到处都用的小组件。样式和原型保持一致。

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "primary" | "ghost" | "danger";
  size?: "md" | "sm";
};

/** 按钮的样式。长得像按钮的链接（比如「开始录音」）也用它。 */
export function buttonClass(variant: NonNullable<ButtonProps["variant"]> = "default", size: NonNullable<ButtonProps["size"]> = "md"): string {
  const variants = {
    default: "border-line-strong bg-white text-ink hover:bg-canvas",
    primary: "border-brand bg-brand text-white hover:bg-brand-hover hover:border-brand-hover",
    ghost: "border-transparent bg-transparent text-ink-2 hover:bg-subtle",
    danger: "border-bad bg-bad text-white hover:opacity-90",
  };
  const sizes = { md: "min-h-10 px-4 text-sm", sm: "min-h-8 px-3 text-[13px]" };
  return `inline-flex shrink-0 cursor-pointer items-center justify-center gap-2 whitespace-nowrap rounded-lg border font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45 ${variants[variant]} ${sizes[size]}`;
}

export function Button({ variant = "default", size = "md", className = "", type = "button", ...rest }: ButtonProps) {
  return <button type={type} className={`${buttonClass(variant, size)} ${className}`} {...rest} />;
}

const inputClass =
  "min-h-10 rounded-lg border border-line-strong bg-white px-3 py-2 text-sm text-ink placeholder:text-muted/70 focus:outline-2 focus:-outline-offset-1 focus:outline-brand disabled:bg-canvas disabled:text-muted";

/** 输入框默认占满一行；调用方自己指定了宽度（w-…）时就用它的。 */
function withWidth(className: string): string {
  return /(^|\s)w-/.test(className) ? className : `w-full ${className}`;
}

export function Input({ className = "", ...rest }: ComponentProps<"input">) {
  return <input className={`${inputClass} ${withWidth(className)}`} {...rest} />;
}

export function Select({ className = "", ...rest }: ComponentProps<"select">) {
  return <select className={`${inputClass} ${withWidth(className)}`} {...rest} />;
}

export function Field({ label, hint, htmlFor, children }: { label: string; hint?: ReactNode; htmlFor: string; children: ReactNode }) {
  return (
    <div>
      <label htmlFor={htmlFor} className="mb-1.5 block text-[13px] font-semibold text-ink-2">
        {label}
      </label>
      {children}
      {hint && <div className="mt-1.5 text-xs leading-relaxed text-muted">{hint}</div>}
    </div>
  );
}

export type Tone = "ok" | "warn" | "bad" | "info" | "mute";

export function Chip({ tone = "mute", children }: { tone?: Tone; children: ReactNode }) {
  const tones = {
    ok: "bg-ok-tint text-ok",
    warn: "bg-warn-tint text-warn",
    bad: "bg-bad-tint text-bad",
    info: "bg-brand-tint text-brand-ink",
    mute: "bg-subtle text-ink-2",
  };
  return (
    <span className={`inline-flex h-6 items-center gap-1.5 whitespace-nowrap rounded-full px-2.5 text-xs font-medium ${tones[tone]}`}>
      {children}
    </span>
  );
}

export function Card({ className = "", children }: { className?: string; children: ReactNode }) {
  return <div className={`rounded-2xl border border-line bg-white ${className}`}>{children}</div>;
}

/** 提示条：成功、警告或出错时显示在内容上方。 */
export function Notice({ tone, children }: { tone: "ok" | "warn" | "bad" | "info"; children: ReactNode }) {
  const tones = {
    ok: "bg-ok-tint text-ok",
    warn: "bg-warn-tint text-warn",
    bad: "bg-bad-tint text-bad",
    info: "bg-brand-tint text-brand-ink",
  };
  return (
    <div role={tone === "bad" ? "alert" : "status"} className={`rounded-xl px-4 py-3 text-sm leading-relaxed ${tones[tone]}`}>
      {children}
    </div>
  );
}

export function ProgressBar({ value }: { value: number }) {
  const pct = Math.round(Math.min(1, Math.max(0, value)) * 100);
  return (
    <div className="h-1.5 overflow-hidden rounded-full bg-line" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div className="h-full rounded-full bg-brand transition-[width] duration-500" style={{ width: `${pct}%` }} />
    </div>
  );
}

export function Spinner({ label }: { label: string }) {
  return (
    <div className="flex items-center justify-center gap-3 p-10 text-sm text-muted" role="status">
      <span className="size-4 animate-spin rounded-full border-2 border-line-strong border-t-brand" aria-hidden="true" />
      {label}
    </div>
  );
}

/** 弹窗。用浏览器原生的 dialog：自带焦点管理，按 Esc 关闭。 */
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);
  return (
    <dialog
      ref={ref}
      onClose={onClose}
      aria-label={title}
      className="m-auto w-[560px] max-w-[calc(100vw-32px)] rounded-2xl p-0 shadow-2xl backdrop:bg-ink/40"
    >
      <div className="flex flex-col gap-4 p-6">
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-bold">{title}</h2>
          <Button variant="ghost" size="sm" aria-label="关闭" onClick={onClose}>
            <Icon name="close" />
          </Button>
        </div>
        {children}
      </div>
    </dialog>
  );
}

const ICONS = {
  list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  settings: "M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1M15 4v4M9 10v4M17 16v4",
  upload: "M12 16V4M7 9l5-5 5 5M5 20h14",
  search: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM20 20l-3.5-3.5",
  close: "M6 6l12 12M18 6 6 18",
  back: "M15 18l-6-6 6-6",
  retry: "M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7",
  edit: "M4 20h4L19 9l-4-4L4 16z",
  check: "M5 12l5 5 9-10",
  file: "M9 18V5l11-2v13M9 18a3 3 0 1 1-6 0 3 3 0 0 1 6 0zM20 16a3 3 0 1 1-6 0 3 3 0 0 1 6 0z",
  logout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
  mic: "M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3zM5 11a7 7 0 0 0 14 0M12 18v3",
  pause: "M9 5v14M15 5v14",
  play: "M7 5l12 7-12 7z",
  stop: "M7 7h10v10H7z",
  flag: "M5 21V4M5 4h11l-2 4 2 4H5",
  download: "M12 4v12M7 11l5 5 5-5M5 20h14",
} as const;

export function Icon({ name, size = 18 }: { name: keyof typeof ICONS; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={ICONS[name]} />
    </svg>
  );
}
