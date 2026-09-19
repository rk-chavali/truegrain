/*
  A small drawn icon set.

  Not Lucide. The Lucide glyph in a tinted rounded square is the stock
  "feature icon" treatment that ships with every shadcn starter, and the
  same eight glyphs turn up in the same eight roles everywhere. These are
  twelve marks on a 16 grid at a single stroke weight, drawn for the
  roles this product actually has.

  They inherit currentColor and carry no accessible name: every one sits
  beside its own text label, so announcing it again would make a screen
  reader say "Explore, Explore".
*/

type Props = { size?: number };

function Svg({ size = 15, children }: Props & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {children}
    </svg>
  );
}

/* A query: a prism splitting one input into grouped output. */
export const IconExplore = (p: Props) => (
  <Svg {...p}>
    <path d="M1.8 8h3.4" />
    <path d="M5.2 8 9 3.4" />
    <path d="M5.2 8 9 8" />
    <path d="M5.2 8 9 12.6" />
    <circle cx="10.6" cy="3.4" r="1.5" />
    <circle cx="10.6" cy="8" r="1.5" />
    <circle cx="10.6" cy="12.6" r="1.5" />
  </Svg>
);

/* The model: strata, the grain a number is true at. */
export const IconModel = (p: Props) => (
  <Svg {...p}>
    <rect x="2" y="2.6" width="12" height="3" rx="0.6" />
    <rect x="2" y="7" width="12" height="2.4" rx="0.6" />
    <rect x="2" y="10.8" width="7.5" height="2.2" rx="0.6" />
  </Svg>
);

/* Activity: a decision trace, one branch refused. */
export const IconActivity = (p: Props) => (
  <Svg {...p}>
    <path d="M1.8 12.2h2.6l1.8-8 2 10 1.9-6.4 1.3 4.4h2.8" />
  </Svg>
);

/* A warehouse connection. */
export const IconConnection = (p: Props) => (
  <Svg {...p}>
    <ellipse cx="8" cy="4" rx="5" ry="2.1" />
    <path d="M3 4v8c0 1.16 2.24 2.1 5 2.1s5-.94 5-2.1V4" />
    <path d="M3 8c0 1.16 2.24 2.1 5 2.1s5-.94 5-2.1" />
  </Svg>
);

export const IconPeople = (p: Props) => (
  <Svg {...p}>
    <circle cx="6.2" cy="5.6" r="2.4" />
    <path d="M1.9 13.4c0-2.3 1.93-3.8 4.3-3.8s4.3 1.5 4.3 3.8" />
    <path d="M11 3.6a2.4 2.4 0 0 1 0 4.5" />
    <path d="M12.3 9.9c1.1.5 1.8 1.5 1.8 3.5" />
  </Svg>
);

/* The deployment: what this instance actually enforces. */
export const IconDeployment = (p: Props) => (
  <Svg {...p}>
    <path d="M8 1.7 13.6 4v4.1c0 3.1-2.3 5.3-5.6 6.2-3.3-.9-5.6-3.1-5.6-6.2V4z" />
    <path d="M5.8 8.1 7.4 9.7l3-3.4" />
  </Svg>
);

/* Checks: a list with each line ticked off. */
export const IconChecks = (p: Props) => (
  <Svg {...p}>
    <path d="M2.4 4.3 3.7 5.6 6 3.1" />
    <path d="M2.4 9.1l1.3 1.3L6 7.9" />
    <path d="M8.3 4.6h5.3" />
    <path d="M8.3 9.4h5.3" />
    <path d="M8.3 13.1h3.4" />
  </Svg>
);

/* Governance: a gate with one bar drawn across it. */
export const IconGovernance = (p: Props) => (
  <Svg {...p}>
    <path d="M8 1.9 13.4 4.3v3.4c0 3-2.2 5.3-5.4 6.4-3.2-1.1-5.4-3.4-5.4-6.4V4.3z" />
    <path d="M4.6 8.2h6.8" />
  </Svg>
);

/* The catalog: a searched index. */
export const IconCatalog = (p: Props) => (
  <Svg {...p}>
    <path d="M2.4 3.2h5.1v10.2H2.4z" />
    <path d="M8.6 3.2h5v4.2h-5z" />
    <circle cx="11" cy="11" r="2.2" />
    <path d="m12.7 12.7 1.3 1.3" />
  </Svg>
);

/* A dashboard: tiles of differing importance. */
export const IconDashboard = (p: Props) => (
  <Svg {...p}>
    <rect x="2" y="2.4" width="6.2" height="5.2" rx="0.6" />
    <rect x="9.6" y="2.4" width="4.4" height="2.4" rx="0.6" />
    <rect x="9.6" y="6.2" width="4.4" height="7.4" rx="0.6" />
    <rect x="2" y="9" width="6.2" height="4.6" rx="0.6" />
  </Svg>
);

/* A saved question. */
export const IconSaved = (p: Props) => (
  <Svg {...p}>
    <path d="M4 2.3h8v11.4l-4-2.6-4 2.6z" />
  </Svg>
);

/* A schedule. */
export const IconSchedule = (p: Props) => (
  <Svg {...p}>
    <circle cx="8" cy="8.4" r="5.6" />
    <path d="M8 5.2v3.4l2.2 1.4" />
  </Svg>
);

/* An alert: a threshold crossed. */
export const IconAlert = (p: Props) => (
  <Svg {...p}>
    <path d="M1.8 11.4 6 5l2.6 3.2L10.4 6l3.8 5.4" />
    <path d="M1.8 13.6h12.4" />
  </Svg>
);

export const IconAccount = (p: Props) => (
  <Svg {...p}>
    <circle cx="8" cy="5.4" r="2.7" />
    <path d="M2.7 13.8c0-2.7 2.37-4.4 5.3-4.4s5.3 1.7 5.3 4.4" />
  </Svg>
);

/* Structural marks, used inline rather than in the rail. */
export const IconChevron = (p: Props) => (
  <Svg {...p}>
    <path d="m6 3.5 5 4.5-5 4.5" />
  </Svg>
);

export const IconClose = (p: Props) => (
  <Svg {...p}>
    <path d="m4 4 8 8M12 4l-8 8" />
  </Svg>
);

export const IconSearch = (p: Props) => (
  <Svg {...p}>
    <circle cx="7.2" cy="7.2" r="4.6" />
    <path d="m10.6 10.6 3 3" />
  </Svg>
);

export const IconDownload = (p: Props) => (
  <Svg {...p}>
    <path d="M8 2.4v7.4" />
    <path d="m5 7 3 3 3-3" />
    <path d="M2.6 12.4v1.2h10.8v-1.2" />
  </Svg>
);

/*
  The mark: three strata thinning downwards. It reads as a grain, and as
  the thing the product is about, which is the granularity a number is
  true at.
*/
export function Grain({ size = 14 }: Props) {
  return (
    <svg
      className="grain"
      width={size}
      height={size}
      viewBox="0 0 16 16"
      aria-hidden="true"
      focusable="false"
    >
      <rect x="0.5" y="1.5" width="15" height="3.6" rx="1" fill="var(--accent)" />
      <rect x="0.5" y="6.7" width="15" height="2.4" rx="1" fill="var(--accent)" opacity="0.6" />
      <rect
        x="0.5"
        y="10.7"
        width="15"
        height="1.4"
        rx="0.7"
        fill="var(--accent)"
        opacity="0.32"
      />
    </svg>
  );
}
