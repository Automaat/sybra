# Synapse Design Guidelines

## Theme: Amber Command

Warm-dark + amber accent. Light mode reads as parchment + gold. Think mission control, not generic SaaS.

**Theme file:** `frontend/src/theme-amber.css`  
**data-theme:** `amber`

---

## Color Palette

### Hue Map

| Slot | Hue | Degrees | Assigned to |
|---|---|---|---|
| `primary` | Amber | 55deg | `in-progress` / brand accent / interactive |
| `secondary` | Teal | 190deg | `testing` / active-AI indicator |
| `tertiary` | Warm-muted | 36deg | `planning` / `new` / `plan-review` |
| `success` | Jade | 156deg | `done` |
| `warning` | Violet | 278deg | `in-review` (reassigned — amber is brand, not warning) |
| `error` | Coral | 22deg | `human-required` |
| `surface` | Warm | 33deg | backgrounds / `todo` / neutral chrome |

### Dark Mode Surfaces

| Role | OKLCH | Hex | Usage |
|---|---|---|---|
| Background | `oklch(9% 0.008 32deg)` | `#110e0b` | Page bg (`surface-950`) |
| Card | `oklch(18% 0.009 34deg)` | `#231c16` | Task cards, panels (`surface-800`) |
| Elevated | `oklch(27% 0.010 35deg)` | `#352c22` | Modals, popovers (`surface-700`) |
| Border | `oklch(36% 0.010 36deg)` | `#453c30` | Dividers, outlines (`surface-600`) |
| Text muted | `oklch(58% 0.009 34deg)` | `#7a7064` | Secondary labels (`surface-500`) |
| Text secondary | `oklch(76% 0.007 32deg)` | `#b5ae9e` | Metadata, timestamps (`surface-300`) |
| Text primary | `oklch(93% 0.004 30deg)` | `#eee8df` | Titles, body (`surface-50`) |

### Light Mode Surfaces

| Role | OKLCH | Hex | Usage |
|---|---|---|---|
| Background | `oklch(98% 0.003 30deg)` | `#faf8f7` | Page bg (`surface-50`) |
| Card | `oklch(96% 0.004 31deg)` | `#f4f1f0` | Task cards, panels (`surface-100`) |
| Elevated | `oklch(93% 0.005 32deg)` | `#eeeae9` | Modals, popovers (`surface-200`) |
| Border | `oklch(88% 0.007 33deg)` | `#dcd6d4` | Dividers, outlines (`surface-300`) |
| Text muted | `oklch(55% 0.009 34deg)` | `#77706e` | Secondary labels |
| Text secondary | `oklch(35% 0.010 33deg)` | `#403937` | Metadata |
| Text primary | `oklch(15% 0.008 32deg)` | `#0e0a09` | Titles, body |

### Accent — Amber

| Stop | OKLCH | Use |
|---|---|---|
| 500 | `oklch(74% 0.18 55deg)` | Brand / on dark backgrounds |
| 600 | `oklch(67% 0.17 57deg)` | Interactive on light bg |
| 700 | `oklch(55% 0.15 59deg)` | Hover state / link text (light mode) |
| 400 | `oklch(79% 0.17 54deg)` | Hover state (dark mode) |
| 200 | `oklch(88% 0.12 52deg)` | Light mode badge background |
| 800 | `oklch(42% 0.13 61deg)` | Light mode badge text |

---

## Status Badge Colors

Badges use Skeleton semantic Tailwind classes — no hardcoded hex needed.

### Class Pattern

| Mode | bg | text |
|---|---|---|
| Light | `bg-{slot}-200` | `text-{slot}-800` |
| Dark | `dark:bg-{slot}-700` | `dark:text-{slot}-200` |

### Per Status

| Status | Skeleton slot | Light bg | Light text | Dark bg | Dark text |
|---|---|---|---|---|---|
| `in-progress` | `primary` | amber-200 | amber-800 | amber-700 | amber-200 |
| `in-review` | `warning` | violet-200 | violet-800 | violet-700 | violet-200 |
| `testing` | `secondary` | teal-200 | teal-800 | teal-700 | teal-200 |
| `planning` / `new` | `tertiary` | warm-200 | warm-800 | warm-700 | warm-200 |
| `human-required` | `error` | coral-200 | coral-800 | coral-700 | coral-200 |
| `done` | `success` | jade-200 | jade-800 | jade-700 | jade-200 |
| `todo` | `surface` | surface-200 | surface-800 | surface-700 | surface-200 |

---

## Visualization Links

| Mode | Tool | Link |
|---|---|---|
| Dark | Coolors | https://coolors.co/040202-0a0605-fe860f-00a292-6567e7-009f4b-e93954-eae7e6 |
| Dark | Realtime Colors | https://www.realtimecolors.com/?colors=eae7e6-040202-fe860f-00a292-6567e7&fonts=Inter-Inter |
| Light | Coolors | https://coolors.co/faf8f7-f4f1f0-c35600-006f63-4544ab-006422-9d2a39-0e0a09 |
| Light | Realtime Colors | https://www.realtimecolors.com/?colors=0e0a09-faf8f7-c35600-006f63-4544ab&fonts=Inter-Inter |

---

## Typography

- Font: system UI stack (`ui-sans-serif, system-ui, -apple-system`) — native on every platform
- Heading weight: 600
- Heading letter-spacing: 0.015em
- Body: normal weight, 0em letter-spacing
- Responsive base: 16px mobile / 18px desktop (768px+)

---

## Spacing & Shape

- Spacing unit: 0.25rem (Tailwind default)
- Base radius: 0.375rem (sm — for badges, inputs, small elements)
- Container radius: 0.75rem (lg — for cards, modals, panels)
- Border width: 1px

---

## Dark Mode Implementation

Dark mode is applied via `.dark` class on `document.documentElement` (not via data-theme change). Controlled from `frontend/src/main.ts`.

- System preference detected via `window.matchMedia`
- User override persisted in `localStorage` as `colorScheme` (`'system' | 'light' | 'dark'`)
- Custom Tailwind variant: `@custom-variant dark (&:where(.dark, .dark *))`

All dark variants use `dark:` prefix: e.g. `bg-surface-100 dark:bg-surface-900`

---

## Anti-patterns

- Never use hardcoded hex/rgb colors in components — always use Skeleton semantic tokens
- Never use `bg-yellow-*` or `bg-orange-*` as "warning" — `warning` = violet in this theme
- Never use `bg-red-*` for human-required — use `bg-error-*` (coral, not red)
- Never use pure `#000000` or `#ffffff` backgrounds — the warm surface tokens handle this
- Don't use high-chroma amber for text on light backgrounds — use 700/800 stops for legibility

---

## Color Research

Full research basis in `docs/color-palette.md` and `docs/ux-research.md`.
