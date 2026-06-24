// A board card associates a task with a project. Rendered as a loud filled
// label it dominated the card and burned the one saturated colour that should
// mean "act on me". These helpers demote it to a quiet cue: the owner prefix
// (constant repeated ink) is dropped from the visible label, and a small dot
// keyed to the project gives an at-a-glance, restrained distinction.

/** Drop the constant `owner/` prefix; the full id stays in the tooltip. */
export function projectShortName(projectId: string): string {
  const slash = projectId.lastIndexOf('/')
  return slash >= 0 ? projectId.slice(slash + 1) : projectId
}

/**
 * A stable, muted dot colour keyed to the project id. Inline OKLCH (fixed
 * lightness/chroma, hashed hue) so it stays restrained — the low chroma keeps
 * it from competing with the saturated amber action colour even if the hashed
 * hue lands near amber — and isn't subject to Tailwind class purging. Lightness
 * is a percentage to match the theme's `oklch(…%)` tokens.
 */
export function projectDotStyle(projectId: string): string {
  let h = 0
  for (let i = 0; i < projectId.length; i++) {
    h = (Math.imul(h, 31) + projectId.charCodeAt(i)) >>> 0
  }
  const hue = h % 360
  return `background-color: oklch(68% 0.11 ${hue}deg)`
}
