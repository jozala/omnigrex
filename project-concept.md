# Autonomous AI Team for Software Development

## 1. Project idea

The goal of the project is to create a platform that enables building **autonomous teams of AI agents responsible for software development**.

Instead of a single general-purpose coding agent handling a task from start to finish, the system consists of multiple specialized agents corresponding to roles found in a traditional software development team:

- Product Owner,
- Architect,
- Developer,
- Reviewer,
- QA,
- potentially others as well, depending on the needs of the project.

Each agent:

- has a clearly defined role and scope of responsibility,
- has its own instructions and persona,
- has access to project knowledge,
- has a separate working session for a given task,
- can communicate with other agents,
- can escalate questions and decisions to a human.

However, the key part of the project is not the agents themselves, but **the system that manages their collaboration, memory, communication, and workflow**.

---

## 2. The human role

The system does not assume that humans are removed entirely.

A human acts as the **supervisor** of the AI team.

Their main responsibilities are:

- initiating new features,
- making important product decisions,
- answering questions that agents cannot resolve on their own,
- approving high-risk decisions,
- observing how the team works,
- configuring the composition and characteristics of the agent team.

The human should not, however, be involved in every minor decision.

The system should resolve as much as possible autonomously and involve the human only when it is genuinely necessary.

---

## 3. GitHub as the main interface and source of truth

GitHub is a central part of the entire solution.

It should not serve only as a code repository, but also as **the project's durable memory and a record of its decision-making process**.

The main artifacts are:

- Issues,
- Pull Requests,
- comments,
- documentation,
- ADRs,
- implementation plans,
- product decisions,
- architecture decisions,
- review results,
- test results.

This means that important knowledge remains in the project even after an agent session ends.

A new agent does not need to rely on the history of a previous session. It can reconstruct the necessary context from the project's durable artifacts.

---

# 4. Example workflow

## 4.1. Creating a task

The supervisor creates a GitHub Issue.

Initially, the Issue may contain only a simple description of intent, for example:

> I would like to allow users to sign in with Google.

It does not need to be a complete specification.

An appropriate label indicating the start of the process is added to the Issue.

Changing the label generates a webhook to the system.

---

## 4.2. Router / orchestrator

The webhook is sent to the central component responsible for orchestrating the process.

The router analyzes:

- the repository,
- the Issue,
- its current status,
- the label,
- the workflow history.

Based on this, it starts the appropriate agent.

A label or another status mechanism can effectively represent **the state of the workflow state machine**.

---

# 5. Product Owner Agent

The first agent may be the Product Owner.

Its task is to analyze the feature from a product perspective.

Among other things, it should:

- understand the user's intent,
- check whether the feature fits the product,
- identify ambiguities,
- find conflicts with existing functionality,
- identify product risks,
- define the behavior of the feature,
- create acceptance criteria,
- identify edge cases.

The Product Owner **should not design the technical implementation**.

If it needs information from the supervisor, it publishes questions in the Issue.

The supervisor can answer directly on GitHub.

The agent then continues working in the same session.

Once the analysis is complete, the Product Owner updates the Issue so that it contains complete business requirements.

It then changes the task state, which triggers the next stage.

---

# 6. Architect Agent

The next stage is technical analysis.

The Architect receives:

- requirements created by the Product Owner,
- project knowledge,
- the existing architecture,
- the codebase,
- ADRs,
- previous technical decisions.

Its task is to determine how the feature should be implemented.

It may prepare:

- an implementation plan,
- a list of components that need to be changed,
- proposed APIs,
- changes to the data model,
- a migration strategy,
- risks,
- a testing plan,
- architecture decisions.

---

# 7. Adaptive workflow

The workflow should not be completely rigid.

Not every task requires the full process:

`Product Owner → Architect → Developer → Reviewer → QA`

The Architect should be able to assess the complexity of the task.

For example:

### Simple task

The Architect may conclude:

> The change is local and does not require architectural decisions.

The task can then go directly to the Developer.

### Medium-complexity task

The Architect prepares a short implementation plan.

### Large task

The Architect may conclude that the Issue is too large.

It can then:

- split it into several Issues,
- propose an implementation order,
- create dependencies between tasks.

This makes the workflow **dynamic and dependent on the nature of the problem**.

---

# 8. Communication between agents

Agents must be able to consult with one another.

Example:

During analysis, the Architect discovers a problem:

> It is unclear whether a user can have both a local account and a Google account at the same time.

It should not make a product decision on its own.

It should be able to perform an operation such as:

`ask_product_owner(question)`

The system then starts the Product Owner **in its existing session associated with that Issue**.

As a result, the Product Owner still has the context of its earlier work.

It can answer the Architect, and the Architect can continue the analysis.

---

# 9. Agent sessions

Each role has its own session associated with a specific Issue.

For example, Issue `#143` may have:

- `product-owner / issue-143`
- `architect / issue-143`
- `developer / issue-143`
- `reviewer / issue-143`
- `qa / issue-143`

The sessions are independent, but they belong to the same process.

If the Reviewer needs to talk to the Product Owner again near the end of the implementation, the system can resume:

`product-owner / issue-143`

instead of creating a new session.

For Issue `#144`, however, a completely new set of sessions is created.

This prevents context from different tasks from being mixed together.

---

# 10. Escalation to a human

Not every discussion between agents has to end with a solution.

If:

- the agents cannot reach agreement,
- there is an important product ambiguity,
- a decision carries significant risk,
- a business decision is required,
- the discussion starts looping without progress,

the system should escalate the matter to the supervisor.

Escalation happens on GitHub.

The Issue may receive an appropriate status such as:

`needs-human-decision`

The agent publishes:

- the problem,
- possible solutions,
- pros and cons,
- its recommendation,
- a specific question for the human.

Once an answer is provided, the workflow resumes.

---

# 11. Developer Agent

Once the requirements and technical plan are ready, the Developer takes over the task.

It receives, among other things:

- the Issue,
- the requirements,
- acceptance criteria,
- the technical plan,
- project documentation,
- relevant ADRs.

The Developer implements the feature.

The work happens on a branch associated with the Issue.

The Developer can:

- modify code,
- add tests,
- run the build,
- run tests,
- analyze failures,
- update documentation.

The result is a Pull Request linked to the Issue.

---

# 12. Reviewer Agent

Once implementation is complete, the Reviewer is started.

The Reviewer should operate as an independent agent, not as another phase of the Developer's session.

It analyzes:

- the requirements,
- the architecture plan,
- the diff,
- the tests,
- the existing architecture,
- project standards.

It can:

- approve the PR,
- raise review comments,
- ask the Developer for changes,
- consult a decision with the Architect,
- ask the Product Owner about product intent.

This creates a real loop:

`Developer ↔ Reviewer`

with the ability to consult other roles when needed.

---

# 13. QA Agent

QA can be another independent role.

It should not merely check whether unit tests are green.

It should analyze the feature from the user's perspective.

It can:

- create test scenarios,
- run integration tests,
- launch the application,
- test APIs,
- perform UI tests,
- check edge cases,
- verify acceptance criteria.

If it finds a problem, the task goes back to the Developer.

---

# 14. Durable project memory

One of the most important problems in the entire system is the limited context window of AI models.

An agent session cannot be the only place where knowledge is stored.

The project should therefore have **layered memory**.

## Layer 1 — agent session

Short-term memory related to the current Issue.

It contains the detailed work history of a particular role.

## Layer 2 — Issue memory

Knowledge related to a specific feature.

It may include:

- requirements,
- comments,
- decisions,
- implementation plan,
- review history.

## Layer 3 — project memory

Durable knowledge that applies independently of a specific task.

Examples:

- product description,
- domain knowledge,
- architecture,
- coding conventions,
- ADRs,
- UX principles,
- technical constraints,
- security rules,
- important product decisions.

---
# 15. Repository as organizational memory

Important knowledge should live as close to the code as possible.

An example structure could look like this:

```text
.ai-team/
    team.md

    agents/
        product-owner.md
        architect.md
        developer.md
        reviewer.md
        qa.md

    product/
        vision.md
        principles.md
        domain.md

    engineering/
        architecture.md
        conventions.md
        testing.md

    decisions/
        product/
        architecture/

docs/
    adr/
```

The exact structure still needs to be designed.

The important principle, however, is:

> An agent starting a new session should be able to reconstruct the most important project context from the repository.

---

# 16. Do not store everything

The system should not store the agents' entire reasoning process.

That would quickly create a huge amount of:

- noise,
- outdated information,
- conflicting hypotheses,
- analysis that is no longer relevant.

Durable memory should primarily contain **the outcomes of the reasoning process**, not the entire process itself.

For example:

- the decision that was made,
- the rationale,
- important alternatives that were considered,
- the consequences of the decision.

---

# 17. Decisions as artifacts

ADRs can serve as a useful pattern.

A similar mechanism can also be used for product decisions.

Example document:

```markdown
# Product Decision: Multiple authentication providers

## Context

Users can sign in locally or through external providers.

## Decision

A single user account can have multiple authentication methods.

## Why

This prevents duplicate accounts from being created.

## Consequences

An identity-provider linking mechanism needs to be implemented.
```

This means that a new Product Owner Agent several months later does not need to read hundreds of old Issues.

---

# 18. Hiring — building the team

One important part of the product may be the **Hiring** process.

After installing the system, the user configures their team of agents.

The platform may first analyze the repository:

- languages,
- frameworks,
- architecture,
- project size,
- tests,
- CI/CD,
- documentation.

Based on this, it can propose a team structure.

For example:

```text
Recommended team

Product Owner
Software Architect
Backend Developer
Frontend Developer
Security Reviewer
QA Engineer
```

The user can:

- add roles,
- remove roles,
- modify their responsibilities,
- change AI models,
- define the level of autonomy.

---

# 19. Agent onboarding

Once the team has been created, **onboarding** takes place.

The system analyzes the existing project and prepares the durable knowledge the agents need.

It may create, among other things:

- a product description,
- an architecture map,
- coding rules,
- a testing strategy,
- a deployment description,
- a domain description,
- the most important constraints.

Each agent also receives a description of its role.

For example:

```markdown
# Role: Software Architect

You are responsible for technical design.

You should:

- understand existing architecture,
- minimize unnecessary complexity,
- identify architectural risks,
- create ADRs for significant decisions,
- consult Product Owner about product decisions.

You should not:

- invent product requirements,
- implement large features yourself,
- change product behavior without consultation.
```

These definitions are part of the repository and evolve together with the project.

---

# 20. Dynamic team composition

Eventually, the team does not have to be fixed.

The system can select roles depending on the task.

For example:

### Small fix

```text
Developer
↓
Reviewer
```

### Standard feature

```text
Product Owner
↓
Architect
↓
Developer
↓
Reviewer
↓
QA
```

### Security-related change

```text
Product Owner
↓
Architect
↓
Security Engineer
↓
Developer
↓
Security Reviewer
↓
QA
```

The system therefore does more than manage the work of agents.

**It dynamically assembles the right team for a given problem.**

---

# 21. Agent skills and tools

An agent should not communicate with the rest of the system only through text.

It should have a set of well-defined tools.

For example:

```text
ask_agent(role, question)

ask_human(question)

create_decision(type, content)

update_issue(...)

create_pull_request(...)

request_review(...)

run_tests(...)

delegate(role, task)

split_issue(...)

mark_ready_for_implementation(...)
```

Such an API makes it possible to control communication and observe the flow of work.

---

# 22. Orchestrator as the key component

The orchestrator is the central architectural component.

It is responsible for:

- receiving webhooks,
- starting agents,
- managing sessions,
- routing messages,
- enforcing the workflow,
- detecting loops,
- enforcing limits,
- handling errors,
- escalating to a human,
- tracking process state.

This is the component that distinguishes the system from a simple collection of prompts for several agents.

---

# 23. Main risks

## 23.1. Context management

Over time, the project will generate an enormous amount of information.

The challenge will be answering the question:

> How do we give an agent exactly the knowledge it needs — neither too little nor too much?

This is likely to be one of the most important problems in the entire project.

---

## 23.2. Information loss between agents

A poorly prepared Product Owner specification leads to a poor Architect plan.

A poor plan leads to a poor implementation.

Therefore, artifacts passed between stages need a defined structure and some form of validation.

---

## 23.3. Loops

A possible scenario:

```text
Reviewer → Developer
Developer → Architect
Architect → Product Owner
Product Owner → Architect
Architect → Developer
Developer → Reviewer
...
```

The system needs:

- iteration limits,
- detection of lack of progress,
- cost limits,
- an escalation mechanism.

---

## 23.4. Incorrect agent consensus

Several agents using similar models may arrive at the same incorrect conclusion.

More agents do not automatically mean greater correctness.

Roles need genuinely different goals and perspectives.

---

## 23.5. Cost

Even a relatively small task may generate many interactions:

```text
PO
Architect
PO consultation
Architect
Developer
Reviewer
Developer
Reviewer
QA
Developer
QA
```

The system will need:

- cost limits per Issue,
- model selection based on the type of work,
- adaptive workflow,
- the ability to skip unnecessary stages.

---

## 23.6. Autonomy versus control

Too little autonomy will mean the user is constantly answering agents.

Too much autonomy may lead to incorrect business and architecture decisions.

The system needs to distinguish between:

- autonomous decisions,
- decisions that require consultation,
- decisions that require human approval.

---

# 24. Potential core product value

The most interesting part of the project does not have to be:

> “AI can write code.”

Many solutions already do that.

A much more interesting value proposition is:

> **A platform that lets you create, hire, onboard, and manage an autonomous AI team that develops a project over the long term and builds its own durable organizational knowledge.**

The system is therefore not just another coding agent.

It is closer to a combination of:

- engineering organization,
- workflow engine,
- agent runtime,
- knowledge management system,
- GitHub automation platform.

---

# 25. Product philosophy

Several principles seem particularly important.

### GitHub remains the source of truth

The user should not be forced to move the entire process into a separate system.

### Knowledge belongs to the project, not to the AI provider

Changing the AI model should not mean losing the organization's memory.

### Agents have roles, not just prompts

A role defines:

- responsibilities,
- permissions,
- goals,
- decision-making boundaries.

### The workflow is adaptive

Not every task follows the same pipeline.

### The human is an exception in the process, not another mandatory stage

The system should ask a human when their decision genuinely adds value.

### Decision outcomes are durable

A future agent should know not only **what exists**, but also **why it was done that way**.

---

# 26. Target vision

The user installs a GitHub App in a repository.

The system analyzes the project.

It then says:

> This looks like a Kotlin/Spring Boot application with React, PostgreSQL, Kubernetes, and a mature CI setup.  
> I recommend the following team.

The user goes through the **Hiring** process.

They configure the agents.

Next comes **Onboarding**.

The agents learn the project and write its organizational knowledge into the repository.

From that point on, the user can create an Issue:

> I would like to allow customers to sign in with Google.

and add the appropriate label.

A few hours later, there may be:

- a refined product specification,
- design decisions,
- an ADR,
- an implementation,
- tests,
- a Pull Request,
- a review,
- a QA report.

The human was involved only where a real decision was required.

Once the task is complete, the knowledge acquired by the team **does not disappear together with the agent sessions**.

It becomes part of the project and can be reused in the next Issue.

---

# 27. The project's most important hypothesis

The most important hypothesis to validate is not:

> Can an LLM write a feature?

We already know that it often can.

The much more important question is:

> **Can we create an autonomous organization of agents that, across many consecutive tasks, can maintain product, architecture, and project-knowledge consistency without constant human supervision?**

If the answer is yes, this may be where the project's greatest value lies.
