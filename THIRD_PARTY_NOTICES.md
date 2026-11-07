# Third-party notices

Omnigrex's own license is in `LICENSE`.
This file records attribution for third-party material copied or adapted into the repository; it is not a complete inventory of package dependencies.
Preserve this file and the referenced license files when distributing the affected material.

## Matt Pocock's grilling skill

- Source: https://github.com/mattpocock/skills
- Revision: `4ae2317ddb22b046e26455e4f9c9b3aaaf62212d`
- Upstream file: `skills/productivity/grilling/SKILL.md`
- Source Git blob: `d2f09a18e6a6c65c55f1271ba08dfeaf06f0bed3`
- Source SHA-256: `a146842ed93ca43d833550fa9adf02e58cb63c83fea9d4b4522995b82242d4e2`
- Affected local file: `internal/agentinstructions/planner.md`, embedded in the Omnigrex binary.
- License: MIT; see [the original copyright and license notice](third-party/mattpocock-grilling/LICENSE).

The pinned source and root license were supplied in Work Item #164 through the scoped Issue read boundary.
The discovery procedure was adapted to bound rounds to five questions, replace sub-agent delegation with local investigation, retain routine-detail authority, and end at planning-only confirmation/revalidation.
The v2 procedure restores the upstream yes-accepts-recommendation requirement, uses ordinary-language examples, and adds Omnigrex short-reference, explicit-assessment and lightweight-checkpoint guidance.
