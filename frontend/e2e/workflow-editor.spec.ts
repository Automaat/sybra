import { test, expect, type Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { load as parseYAML } from "js-yaml";

import { isolatedSybraHome } from "./lib/sybra-home";

const SYBRA_HOME = isolatedSybraHome();
const FIXTURE_ID = "wf-editor-e2e";
const API_BASE = "http://localhost:8080";

/**
 * Seed and read the fixture through the API rather than the workflows
 * directory.
 *
 * The directory is only the store under the `file` backend. Writing a YAML
 * there after the server booted was invisible to every other backend, because
 * those read rows and the one-time file import has already run — which is how
 * this spec failed the moment the default moved to sqlite while every unit
 * suite stayed green. Going through the API exercises whichever backend is
 * configured.
 */
/**
 * The bearer token this home's server runs with.
 *
 * CI writes an explicit `server.auth_token` into the home's config, while a
 * local run leaves it empty and the server generates one into
 * `server_auth_token`. Both are checked so the suite works either way.
 */
async function authToken(): Promise<string> {
  try {
    const cfg = parseYAML(
      await readFile(join(SYBRA_HOME, "config.yaml"), "utf8"),
    ) as { server?: { auth_token?: string } } | undefined;
    const fromConfig = cfg?.server?.auth_token?.trim();
    if (fromConfig) {
      return fromConfig;
    }
  } catch {
    /* no config file, fall through to the generated token */
  }
  return (await readFile(join(SYBRA_HOME, "server_auth_token"), "utf8")).trim();
}

async function api(method: string, args: unknown[]): Promise<Response> {
  const res = await fetch(`${API_BASE}/api/WorkflowService/${method}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${await authToken()}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(args),
  });
  if (!res.ok) {
    throw new Error(`${method} failed: ${res.status} ${await res.text()}`);
  }
  return res;
}

async function ensureFixture() {
  const src = join(import.meta.dirname, "fixtures", "wf-editor-e2e.yaml");
  // SaveWorkflow takes a Definition, so the fixture is parsed here rather than posted as YAML.
  await api("SaveWorkflow", [parseYAML(await readFile(src, "utf8"))]);
}

async function removeFixture() {
  // Absent is the desired end state, so a fixture the test already removed is not a failure.
  try {
    await api("DeleteWorkflow", [FIXTURE_ID]);
  } catch {
    /* already gone */
  }
}

/**
 * The saved definition as raw JSON text.
 *
 * The assertions look for step ids, which appear verbatim in the encoded
 * definition, so the checks hold without re-serializing it to YAML — and they
 * now describe what the board stores rather than what a file on disk holds.
 */
async function savedDefinition(): Promise<string> {
  const res = await api("GetWorkflow", [FIXTURE_ID]);
  return await res.text();
}

async function openWorkflowEditor(page: Page) {
  await page.goto("/");
  await page.locator('[data-part="trigger"]', { hasText: /Workflows/ }).click();
  // Wait for the fixture card to appear and click it.
  const card = page.getByRole("button", { name: /E2E Editor Fixture/ });
  await expect(card).toBeVisible({ timeout: 10_000 });
  await card.click();
  // Detail header should render the workflow name.
  await expect(
    page.locator("h2", { hasText: "E2E Editor Fixture" }),
  ).toBeVisible();
}

test.beforeAll(async () => {
  await ensureFixture();
});

test.afterAll(async () => {
  await removeFixture();
});

// The fixture is hidden from the user-facing list; reveal it for the editor tests.
test.beforeEach(async ({ page }) => {
  await page.addInitScript(() =>
    localStorage.setItem("sybra.showFixtures", "true"),
  );
});

test.describe("Workflow editor — list page", () => {
  test("list card shows trigger event and condition count", async ({
    page,
  }) => {
    await page.goto("/");
    await page
      .locator('[data-part="trigger"]', { hasText: /Workflows/ })
      .click();

    const card = page.getByRole("button", { name: /E2E Editor Fixture/ });
    await expect(card).toBeVisible({ timeout: 10_000 });
    await expect(card).toContainText("trigger: task.created");
    await expect(card).toContainText("1 cond");
    await expect(card).toContainText("1 steps");
  });
});

test.describe("Workflow editor — trigger panel", () => {
  test("renders event and existing condition", async ({ page }) => {
    await openWorkflowEditor(page);

    // Trigger node in graph shows "1 condition" from the fixture.
    await expect(page.getByText(/1 condition/)).toBeVisible();

    // Click the trigger node to open the TriggerConfigPanel sidebar.
    await page.locator(".svelte-flow__node-triggerNode").click();

    // Event dropdown reflects the seeded event.
    await expect(page.locator("select").first()).toHaveValue("task.created");

    // Condition row inputs — target by placeholder attribute (Svelte sets
    // `value` as a DOM property, not an attribute, so input[value="…"]
    // won't match; placeholder is a normal attribute and is reliable).
    await expect(
      page.locator('input[placeholder="task.tags"]').first(),
    ).toHaveValue("task.tags");
    await expect(
      page.locator('input[placeholder="value"]').first(),
    ).toHaveValue("skip");
  });

  test("can add a new trigger condition", async ({ page }) => {
    await openWorkflowEditor(page);

    // Click the trigger node to open the TriggerConfigPanel sidebar.
    await page.locator(".svelte-flow__node-triggerNode").click();

    // Button text is "+ Add" in TriggerConfigPanel.
    const addBtn = page.getByRole("button", { name: "+ Add", exact: true });
    await addBtn.click();

    // After adding, trigger node summary should reflect 2 conditions.
    await expect(page.getByText(/2 conditions/)).toBeVisible();

    // Unsaved badge should appear.
    await expect(page.locator("span", { hasText: "unsaved" })).toBeVisible();
  });
});

test.describe("Workflow editor — add step + transitions", () => {
  test("clicking + Add step creates a new step and opens the config panel", async ({
    page,
  }) => {
    await openWorkflowEditor(page);

    // Panel should not be visible yet (no step selected).
    await expect(
      page.locator("h3", { hasText: "Step Config" }),
    ).not.toBeVisible();

    await page.getByRole("button", { name: "+ Add step", exact: true }).click();

    // Config panel opens with the seeded default name.
    await expect(page.locator("h3", { hasText: "Step Config" })).toBeVisible();
    await expect(page.getByLabel("Name")).toHaveValue("New step");

    // Transitions section is visible and empty by default.
    await expect(
      page.locator("span", { hasText: /^Transitions$/ }),
    ).toBeVisible();
    await expect(
      page.getByText("No transitions — step ends the workflow"),
    ).toBeVisible();

    // Unsaved badge appears after mutation.
    await expect(page.locator("span", { hasText: "unsaved" })).toBeVisible();
  });

  test("can add a transition targeting an existing step", async ({ page }) => {
    await openWorkflowEditor(page);

    // Select the existing step by clicking its graph node.
    await page.locator(".svelte-flow__node-stepNode").first().click();
    await expect(page.locator("h3", { hasText: "Step Config" })).toBeVisible();

    // Transitions section → + Add (exact-match disambiguates from
    // "+ Add step" and "+ Add condition").
    await page.getByRole("button", { name: "+ Add", exact: true }).click();

    // A new transition row with goto dropdown defaulting to <end workflow>.
    const gotoSelect = page
      .locator("select")
      .filter({ hasText: /end workflow/ });
    await expect(gotoSelect).toBeVisible();

    // Toggle conditional (when) checkbox.
    const whenCheckbox = page.getByRole("checkbox", { name: /conditional/ });
    await whenCheckbox.check();
    await expect(whenCheckbox).toBeChecked();

    await expect(page.locator("span", { hasText: "unsaved" })).toBeVisible();
  });
});

test.describe("Workflow editor — save round-trip", () => {
  test("add step + save persists to disk", async ({ page }) => {
    await openWorkflowEditor(page);

    await page.getByRole("button", { name: "+ Add step", exact: true }).click();

    // Change the name to something identifiable.
    const nameInput = page.getByLabel("Name");
    await expect(nameInput).toHaveValue("New step");
    await nameInput.fill("e2e-added-step");
    await nameInput.blur();

    // Save via the header button.
    await page.getByRole("button", { name: /^Save$/ }).click();

    // Unsaved badge should clear.
    await expect(page.locator("span", { hasText: "unsaved" })).not.toBeVisible({
      timeout: 5_000,
    });

    // Read back through the API, which reads whichever backend is configured.
    const yaml = await savedDefinition();
    expect(yaml).toContain("e2e-added-step");
    // Original step still there.
    expect(yaml).toContain("first-step");
  });
});
