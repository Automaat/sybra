#!/usr/bin/env node
// Enforces the pin strategy documented in CONTRIBUTING.md →
// "Frontend Dependency Pin Strategy". Build/test toolchain and pre-release
// runtime libs must use exact pins; stable UI/runtime libs may use caret.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const pkgPath = path.join(__dirname, '..', 'package.json');

const EXACT_NAMES = new Set([
  'vite',
  'vitest',
  'typescript',
  'svelte',
  'svelte-check',
  'oxlint',
  'tailwindcss',
  'jsdom',
]);

const EXACT_PREFIXES = [
  '@vitest/',
  '@playwright/',
  '@skeletonlabs/',
  '@sveltejs/',
  '@tailwindcss/',
  '@testing-library/',
  '@types/',
];

// Pre-release runtime libs — caret on pre-releases is risky because there's
// no semver compatibility guarantee between alpha/beta versions.
const PRE_RELEASE_EXACT = new Set([
  '@wailsio/runtime',
]);

function requiresExact(name) {
  if (PRE_RELEASE_EXACT.has(name)) return true;
  if (EXACT_NAMES.has(name)) return true;
  return EXACT_PREFIXES.some((p) => name.startsWith(p));
}

function isExactVersion(version) {
  return /^\d+\.\d+\.\d+(-[\w.-]+)?$/.test(version);
}

const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
const all = {
  ...(pkg.dependencies ?? {}),
  ...(pkg.devDependencies ?? {}),
};

const violations = [];
for (const [name, version] of Object.entries(all)) {
  if (requiresExact(name) && !isExactVersion(version)) {
    violations.push({ name, version });
  }
}

if (violations.length) {
  console.error('frontend pin strategy violations in frontend/package.json:');
  for (const { name, version } of violations) {
    console.error(`  ${name}: ${version} — must be exact-pinned (no ^/~ range)`);
  }
  console.error('\nSee CONTRIBUTING.md → "Frontend Dependency Pin Strategy".');
  process.exit(1);
}

console.log(`pin strategy OK (${Object.keys(all).length} packages checked).`);
