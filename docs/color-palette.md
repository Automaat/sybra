# Synapse Color Palette Research

> Research covering successful dark-mode developer tools and AI products (2023–2025). All values in OKLCH for Skeleton UI v4 compatibility.

---

## Status Color Mapping (existing)

Current Skeleton slot → task status:
- `primary` → `in-progress`
- `secondary` → `testing`
- `tertiary` → `new` / `planning` / `plan-review`
- `success` → `done`
- `warning` → `in-review`
- `error` → `human-required`
- `surface` → `todo`

---

## Key Research Findings

### Hue conventions in AI dev tools (2024–2025)

- **Indigo/violet 260–280deg** dominates AI product accents: GitHub Copilot, Perplexity, Devin, Cursor — encodes "intelligence/automation"
- **Teal 185–200deg** universally used for "active AI processing" states: Perplexity stream, Claude thinking, Copilot active
- **Amber 50–60deg** is 2024's differentiated alternative: Bun, Astro, Cloudflare — warm, decisive, non-generic

### Dark mode surface tint

- True neutral (C=0): maximum contrast, zero personality — Generic
- Cool blue-gray (C=0.005–0.01, H=245–265°): industry default — GitHub, Cursor, Linear
- Warm-black tint (C=0.005–0.01, H=20–40°): Claude's approach — warm, unusual in dev tools
- Indigo-tinted (C=0.01–0.015, H=270°): Cursor variant — AI-native, cohesive with indigo accent

### Saturation guidelines for dark mode

- Surface chroma: 0–0.02 (above 0.03 = noticeable colored background)
- Accent sweet spot: L 60–72%, C 0.16–0.22
- Badge backgrounds: C 0.05–0.1 at L 25–35%
- Text primary: L 88–95%, C 0–0.02

---

## Palette Options

### A — "Slate Professional"

**Personality**: Linear-meets-GitHub. Cool neutral, single blue accent. The "just ship it" palette.

**Base hue**: 250deg (cool blue-gray), C=0.006–0.010

| Role | OKLCH |
|---|---|
| Background | `oklch(9.5% 0.006 250deg)` |
| Card | `oklch(13% 0.007 252deg)` |
| Border | `oklch(25% 0.009 255deg)` |
| Text primary | `oklch(92% 0.005 250deg)` |
| **Accent (blue)** | `oklch(69% 0.16 245deg)` |

**Status colors (accent hues)**: blue / amber / teal / violet / rose / jade

**Verdict**: Familiar, professional, zero controversy. Won't stand out visually.

---

### B — "Synapse Indigo" ⭐ RECOMMENDED

**Personality**: Linear-precise meets AI-native. Indigo-tinted dark surface (270deg, very low chroma). Single indigo accent at high chroma. Teal secondary for running/testing states.

**Base hue**: 270deg (indigo), C=0.012–0.016 — you feel the cohesion, not the color

| Role | OKLCH |
|---|---|
| Background (`surface-950`) | `oklch(9% 0.012 270deg)` |
| Card (`surface-900`) | `oklch(12.5% 0.013 271deg)` |
| Elevated (`surface-800`) | `oklch(17% 0.013 272deg)` |
| Border default (`surface-700`) | `oklch(26% 0.015 273deg)` |
| Border strong (`surface-600`) | `oklch(35% 0.016 274deg)` |
| Text muted (`surface-400`) | `oklch(58% 0.015 272deg)` |
| Text secondary (`surface-200`) | `oklch(75% 0.012 270deg)` |
| Text primary (`surface-50`) | `oklch(93% 0.006 268deg)` |

**Accent — Indigo (primary)**:
| Stop | OKLCH |
|---|---|
| 500 (main) | `oklch(62% 0.22 270deg)` |
| 400 (hover) | `oklch(68% 0.23 268deg)` |
| 600 (pressed) | `oklch(55% 0.20 272deg)` |

**Full status palette**:
| Status | Slot | Badge bg | Badge text |
|---|---|---|---|
| `in-progress` | `primary` | `oklch(20% 0.12 270deg)` | `oklch(78% 0.18 268deg)` |
| `in-review` | `warning` | `oklch(26% 0.10 55deg)` | `oklch(88% 0.14 58deg)` |
| `testing` | `secondary` | `oklch(20% 0.12 192deg)` | `oklch(75% 0.17 190deg)` |
| `planning`/`new` | `tertiary` | `oklch(20% 0.10 295deg)` | `oklch(76% 0.14 293deg)` |
| `human-required` | `error` | `oklch(24% 0.11 28deg)` | `oklch(82% 0.16 25deg)` |
| `done` | `success` | `oklch(20% 0.12 160deg)` | `oklch(74% 0.17 158deg)` |
| `todo` | `surface` | `oklch(20% 0.013 272deg)` | `oklch(65% 0.012 270deg)` |

**Tailwind class pattern for badges**:
```
in-progress:      bg-primary-900 text-primary-300
in-review:        bg-warning-900 text-warning-300
testing:          bg-secondary-900 text-secondary-300
planning/new:     bg-tertiary-900 text-tertiary-300
human-required:   bg-error-900 text-error-300
done:             bg-success-900 text-success-300
todo:             bg-surface-800 text-surface-400
```

**Why indigo-270deg?** It's the convergent AI product hue in 2025 (Perplexity, Cursor, Devin, Copilot) — not trend-following, but because it reads as "intelligence/automation" to developers using these tools daily. The low-chroma surface tint ties the background to the accent, creating cohesion that pure neutral palettes lack. Current `vox` theme is at 294–302deg (lavender-purple) with excessive chroma (0.03–0.07 at dark stops) — this is a more restrained upgrade in the same hue family.

---

### C — "Amber Command" ⭐ SELECTED

**Personality**: Warm-dark like a mission control terminal. Amber accent — unusual in AI tooling, high visual memory. Think Bun's orange meets Warp's dark. Light mode reads as "parchment + gold" — warm cream surfaces with amber accent.

**Base hue**: 32–35deg (warm-black dark / warm-white light), C=0.003–0.010

**Note**: `warning` slot reassigned to violet (280deg) since amber IS the brand color. `in-review` reads as "pending judgment" via violet.

#### Dark mode surfaces
| Role | OKLCH | Hex |
|---|---|---|
| Background | `oklch(9% 0.008 32deg)` | `#110e0b` |
| Card | `oklch(13% 0.009 33deg)` | `#1a1510` |
| Elevated | `oklch(18% 0.009 34deg)` | `#231c16` |
| Border default | `oklch(27% 0.010 35deg)` | `#352c22` |
| Text muted | `oklch(58% 0.009 34deg)` | `#7a7064` |
| Text secondary | `oklch(76% 0.007 32deg)` | `#b5ae9e` |
| Text primary | `oklch(93% 0.004 30deg)` | `#eee8df` |

#### Light mode surfaces
| Role | OKLCH | Hex |
|---|---|---|
| Background | `oklch(98% 0.003 30deg)` | `#faf8f7` |
| Card | `oklch(96% 0.004 31deg)` | `#f4f1f0` |
| Elevated | `oklch(94% 0.005 32deg)` | `#eeeae9` |
| Border default | `oklch(88% 0.007 33deg)` | `#dcd6d4` |
| Border strong | `oklch(80% 0.009 33deg)` | `#c3bcba` |
| Text muted | `oklch(55% 0.009 34deg)` | `#77706e` |
| Text secondary | `oklch(35% 0.010 33deg)` | `#403937` |
| Text primary | `oklch(15% 0.008 32deg)` | `#0e0a09` |

#### Light mode badge colors
| Status | Badge bg | Badge text |
|---|---|---|
| `in-progress` (amber) | `oklch(94% 0.07 51deg)` `#ffdfc1` | `oklch(32% 0.13 59deg)` `#5f1700` |
| `testing` (teal) | `oklch(93% 0.06 183deg)` `#bcf6eb` | `oklch(30% 0.13 191deg)` `#003f3d` |
| `planning` (warm) | `oklch(95% 0.04 33deg)` `#ffe6de` | `oklch(28% 0.09 38deg)` `#4b1301` |
| `done` (jade) | `oklch(93% 0.06 151deg)` `#ccf4d4` | `oklch(28% 0.12 158deg)` `#00380e` |
| `in-review` (violet) | `oklch(95% 0.04 272deg)` `#e5eeff` | `oklch(28% 0.13 280deg)` `#221766` |
| `human-required` (coral) | `oklch(94% 0.05 14deg)` `#ffdee1` | `oklch(30% 0.14 20deg)` `#62000c` |
| `todo` (neutral) | `oklch(96% 0.004 31deg)` `#f4f1f0` | `oklch(47% 0.010 35deg)` `#605957` |

#### Visualization links
- **Dark — Coolors**: https://coolors.co/040202-0a0605-fe860f-00a292-6567e7-009f4b-e93954-eae7e6
- **Dark — Realtime Colors**: https://www.realtimecolors.com/?colors=eae7e6-040202-fe860f-00a292-6567e7&fonts=Inter-Inter
- **Light — Coolors**: https://coolors.co/faf8f7-f4f1f0-c35600-006f63-4544ab-006422-9d2a39-0e0a09
- **Light — Realtime Colors**: https://www.realtimecolors.com/?colors=0e0a09-faf8f7-c35600-006f63-4544ab&fonts=Inter-Inter

---

### D — "Neutral Edge"

**Personality**: Maximum restraint. True neutral dark, white accent. Zero hue distraction. Typography does the work.

**Base**: `oklch(8.5% 0 0)` — achromatic

**Verdict**: Timeless, works beautifully in dense list views, zero brand differentiation. Status badges pop with maximum contrast against the neutral base.

---

## Full Skeleton Theme CSS — Palette B

Create `frontend/src/theme-synapse.css`:

```css
[data-theme='synapse'] {
  --text-scaling: 1.067;
  --base-font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
  --heading-font-family: ui-sans-serif, system-ui, -apple-system, sans-serif;
  --heading-font-weight: 600;
  --heading-letter-spacing: 0.015em;
  --spacing: 0.25rem;
  --radius-base: 0.375rem;
  --radius-container: 0.75rem;
  --default-border-width: 1px;
  --body-background-color-dark: oklch(9% 0.012 270deg);

  /* primary: indigo 270deg */
  --color-primary-50:  oklch(96% 0.05 264deg);
  --color-primary-100: oklch(90% 0.08 266deg);
  --color-primary-200: oklch(82% 0.12 267deg);
  --color-primary-300: oklch(76% 0.18 266deg);
  --color-primary-400: oklch(68% 0.23 268deg);
  --color-primary-500: oklch(62% 0.22 270deg);
  --color-primary-600: oklch(55% 0.20 272deg);
  --color-primary-700: oklch(43% 0.17 274deg);
  --color-primary-800: oklch(32% 0.14 276deg);
  --color-primary-900: oklch(22% 0.11 278deg);
  --color-primary-950: oklch(14% 0.09 280deg);

  /* secondary: teal 190deg */
  --color-secondary-50:  oklch(95% 0.05 186deg);
  --color-secondary-100: oklch(88% 0.09 187deg);
  --color-secondary-200: oklch(80% 0.13 188deg);
  --color-secondary-300: oklch(78% 0.16 188deg);
  --color-secondary-400: oklch(70% 0.19 189deg);
  --color-secondary-500: oklch(60% 0.20 190deg);
  --color-secondary-600: oklch(50% 0.17 191deg);
  --color-secondary-700: oklch(40% 0.15 192deg);
  --color-secondary-800: oklch(30% 0.12 193deg);
  --color-secondary-900: oklch(20% 0.11 192deg);
  --color-secondary-950: oklch(12% 0.09 193deg);

  /* tertiary: lavender 293deg */
  --color-tertiary-50:  oklch(96% 0.04 289deg);
  --color-tertiary-100: oklch(90% 0.07 290deg);
  --color-tertiary-200: oklch(82% 0.10 291deg);
  --color-tertiary-300: oklch(78% 0.12 291deg);
  --color-tertiary-400: oklch(70% 0.14 292deg);
  --color-tertiary-500: oklch(58% 0.16 293deg);
  --color-tertiary-600: oklch(48% 0.14 294deg);
  --color-tertiary-700: oklch(38% 0.12 295deg);
  --color-tertiary-800: oklch(28% 0.10 296deg);
  --color-tertiary-900: oklch(20% 0.09 296deg);
  --color-tertiary-950: oklch(13% 0.07 297deg);

  /* success: jade 158deg */
  --color-success-50:  oklch(95% 0.05 154deg);
  --color-success-100: oklch(88% 0.09 155deg);
  --color-success-200: oklch(80% 0.13 156deg);
  --color-success-300: oklch(78% 0.14 156deg);
  --color-success-400: oklch(70% 0.17 157deg);
  --color-success-500: oklch(60% 0.19 158deg);
  --color-success-600: oklch(50% 0.16 159deg);
  --color-success-700: oklch(40% 0.14 160deg);
  --color-success-800: oklch(30% 0.12 161deg);
  --color-success-900: oklch(20% 0.10 160deg);
  --color-success-950: oklch(12% 0.08 161deg);

  /* warning: gold 55deg */
  --color-warning-50:  oklch(97% 0.04 51deg);
  --color-warning-100: oklch(92% 0.08 52deg);
  --color-warning-200: oklch(86% 0.12 53deg);
  --color-warning-300: oklch(88% 0.13 53deg);
  --color-warning-400: oklch(80% 0.16 54deg);
  --color-warning-500: oklch(72% 0.18 55deg);
  --color-warning-600: oklch(62% 0.16 56deg);
  --color-warning-700: oklch(50% 0.14 58deg);
  --color-warning-800: oklch(38% 0.12 59deg);
  --color-warning-900: oklch(26% 0.10 60deg);
  --color-warning-950: oklch(16% 0.08 61deg);

  /* error: coral 25deg */
  --color-error-50:  oklch(96% 0.05 20deg);
  --color-error-100: oklch(90% 0.09 21deg);
  --color-error-200: oklch(82% 0.14 22deg);
  --color-error-300: oklch(82% 0.15 22deg);
  --color-error-400: oklch(74% 0.18 23deg);
  --color-error-500: oklch(62% 0.20 25deg);
  --color-error-600: oklch(52% 0.17 27deg);
  --color-error-700: oklch(42% 0.15 28deg);
  --color-error-800: oklch(32% 0.12 29deg);
  --color-error-900: oklch(23% 0.10 30deg);
  --color-error-950: oklch(14% 0.08 31deg);

  /* surface: indigo-tinted dark 270deg */
  --color-surface-50:  oklch(95% 0.006 268deg);
  --color-surface-100: oklch(88% 0.009 269deg);
  --color-surface-200: oklch(80% 0.012 270deg);
  --color-surface-300: oklch(70% 0.013 271deg);
  --color-surface-400: oklch(58% 0.015 272deg);
  --color-surface-500: oklch(46% 0.016 274deg);
  --color-surface-600: oklch(35% 0.016 274deg);
  --color-surface-700: oklch(26% 0.015 273deg);
  --color-surface-800: oklch(17% 0.013 272deg);
  --color-surface-900: oklch(12.5% 0.013 271deg);
  --color-surface-950: oklch(9% 0.012 270deg);
}
```

### Wiring it up

In `frontend/src/style.css`, replace:
```css
/* @import '@skeletonlabs/skeleton/themes/vox'; */
@import './theme-synapse.css';
```

In root HTML element or `App.svelte`, change:
```html
<!-- data-theme="vox" → data-theme="synapse" -->
```

---

## Semantic Design Rationale (Palette B)

| Slot | Hue | Why this color |
|---|---|---|
| `primary` / `in-progress` | Indigo 270deg | Brand accent = active/running — most visible state |
| `secondary` / `testing` | Teal 190deg | Universal "AI actively processing" color in 2025 |
| `tertiary` / `planning` | Lavender 293deg | Slightly cooler than indigo = "thinking, not yet running" |
| `warning` / `in-review` | Gold 55deg | Review gate = awaiting judgment, not dangerous |
| `error` / `human-required` | Coral 25deg | Urgency without red-danger connotation |
| `success` / `done` | Jade 158deg | Modern completion color (Stripe uses it too) |
| `surface` / `todo` | Indigo-tinted neutral | Invisible until needed — the "not yet" state |

---

## Sources

- Linear design system (LCH migration writeup)
- Radix Colors documentation (12-step perceptual scale)
- Skeleton UI v4 theme API
- OKLCH.com reference
- Vercel/v0.dev, GitHub dark mode, Cursor, Warp visual inspection
- Perplexity, Devin, Claude.ai color analysis
