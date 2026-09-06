# Health transition causes

The local leader owns classification and recovery policy; the standalone worker only executes agents. Health reports do not infer planning quality from `headless`: it is the only supported execution transport.

Escalations into `blocked` or unexpected `human-required` carry a structured cause and suggested next action. Classification requires a reason code, failure owner, and authoritative evidence provenance. Machine/external-transient owners mean infrastructure; specification owners with a `planning.*` code mean a planning-stage failure to investigate, not proof that the requirements were bad. Other specification and operator/policy cases remain distinct. Missing, legacy, or provider-only evidence is explicitly `unknown`. Expected manual/draft review gates remain excluded.

Task transition hooks carry the actor with the exact written snapshot, avoiding a race with the next transition's actor. Only the explicit `monitor.incident.reopen` actor moving a completed/cancelled task to todo starts a new incident episode. These recurrences appear as informational findings, do not lower the health score by themselves, and reset ordinary bounce history. Repeated transitions within an episode still warn; historical reopenings without actor metadata remain unknown, not silently reclassified as healthy.

The self-monitor's obsolete `triage_mismatch -> flip_agent_mode` action is retired: setting an already-headless task to headless and queuing another run repairs nothing. Findings and judgments remain reportable, but do not authorize an automatic retry. Existing incident repair routing and work-project scrubbing are unchanged. This change enables no paid judge, changes no deployment configuration, and does not override dry-run settings.
