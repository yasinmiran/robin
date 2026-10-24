import {
  LOCKUP_WIDTH,
  OPENING,
  POINTER,
  TILE,
  WORDMARK,
  WORDMARK_BASELINE,
  WORDMARK_X,
} from "@/lib/logo";

interface LogoProps {
  /** Rendered height in px. */
  size?: number;
  className?: string;
  /** Accessible name; without one the logo is decorative. */
  title?: string;
}

function a11y(title: string | undefined) {
  return title ? { role: "img", "aria-label": title } : { "aria-hidden": true as const };
}

function MarkShapes() {
  return (
    <>
      <path fill="var(--tile)" d={TILE} />
      <path fill="var(--mint)" d={OPENING} />
      <path fill="var(--tile)" d={POINTER} />
    </>
  );
}

/** Mark draws the tile, the opening and the pointer in the theme's colours. */
export function Mark({ size = 24, className, title }: LogoProps) {
  return (
    <svg
      viewBox="0 0 32 32"
      width={size}
      height={size}
      className={className}
      focusable="false"
      {...a11y(title)}
    >
      <MarkShapes />
    </svg>
  );
}

/** Lockup is the mark plus the outlined wordmark, which takes currentColor. */
export function Lockup({ size = 24, className, title }: LogoProps) {
  return (
    <svg
      viewBox={`0 0 ${LOCKUP_WIDTH} 32`}
      width={(size * LOCKUP_WIDTH) / 32}
      height={size}
      className={className}
      focusable="false"
      {...a11y(title)}
    >
      <MarkShapes />
      <path
        fill="currentColor"
        transform={`translate(${WORDMARK_X} ${WORDMARK_BASELINE})`}
        d={WORDMARK}
      />
    </svg>
  );
}
