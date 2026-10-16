---
name: write-docs-page
description: Write or revise a page in the NVKE documentation site (docs/, rendered with Fern). Use when drafting a new docs page, rewriting a scaffold page, or checking a page against the documentation conventions. Covers page types, voice, terminology, Markdown and Fern conventions, and how to verify every claim against the code.
---

# Write an NVKE docs page

The documentation site is built from Markdown under `docs/` and rendered by
Fern (`fern/docs.yml`, navigation in `docs/index.yml`). Pages are read on the
site and on GitHub, so they must render correctly in both.

The model page is [docs/overview/what-is-nvke.md](../../../docs/overview/what-is-nvke.md).
Read it before you write. Match its voice, density and structure.

## 1. Before you write

1. Read the model page.
2. Read the page you are replacing, if one exists, including any
   "Source material" excerpts in it. Excerpts are input, not output.
3. Read the source files the excerpts cite, at the current commit. The code is
   the authority: when an excerpt and the code disagree, the code wins, and you
   say so in your summary.
4. List every page you intend to link to, and check that each one exists in
   `docs/index.yml` or is being written in the same change.

## 2. Page types

Every page is exactly one type. Do not mix them.

| Type | Answers | Shape |
|---|---|---|
| Explanation | What is it and why? | Definition first, then sections that each answer one question. No procedures. |
| Tutorial | Can I get a first success? | One path, numbered steps, a visible result at the end. No options or branches. |
| How-to | How do I do this task? | Goal, prerequisites, numbered steps, verification, next steps. |
| Reference | What exactly is supported or available? | Tables and lists, no narrative. Generated from source where possible. |

### How-to template

<!-- Four backticks on purpose: the template holds ``` blocks, which would close a three-backtick fence early. -->
````markdown
# <Verb phrase: Bootstrap the management cluster>

This page describes how to <task>. <When you do it: after which page, before which page.>

## Before you begin

- <Pages the reader must have completed, in path order, first: "You have read Plan your installation", as a link to that page.>
- <Then each remaining prerequisite, with a link to the page that satisfies it.>

## <Step group, if the task has phases>

1. <One action per step. Imperative mood.>

   ```bash
   <command the reader can paste>
   ```

   <What they should see, or how to tell the step worked.>

## Verify

<How to confirm the whole task succeeded.>

## Next steps

- <One to four links, in the order the reader needs them.>
````

A how-to may include one short "What happens during `<task>`" section (a
diagram and a paragraph) when later steps, failure handling or verification
depend on the reader understanding the sequence. Keep it after the main steps
and before failure handling. Anything longer belongs on an explanation page.

The tutorial template is the same, but with no options and a single path.
Explanation and reference pages follow the model page.

## 3. Voice

- Address the reader as "you". Use the imperative in steps ("Run", "Set",
  "Confirm").
- Present tense. Describe what the software does now. No roadmap, no
  "currently", "soon", "will be able to", "new".
- Open with the conclusion: the first sentence of a page or section says the
  thing the reader most needs.
- Be specific. Replace "significantly", "robust", "simple", "easy",
  "seamless", "powerful", "just" and "leverage" with the fact, or delete them.
- No metaphors where a literal word works.
- Use at most one em dash in a paragraph, and none in headings.
- Number of list items matches the content, not a rhythm of three.
- Spell out an acronym on its first use on each page, with the acronym in
  parentheses: "virtual IP address (VIP)". Well-known terms (API, CLI, IP,
  CPU, GPU, TPM, YAML) need no expansion.
- The page is not the actor. Write "This page describes how to..." or address
  the reader, not "This page turns...".
- No "we" except in the preview statement on the model page.
- Do not repeat preview caveats. Link to
  [Preview status and known limitations](../../../docs/overview/preview-status.md)
  where a limitation affects the task, in one sentence.

## 4. Terminology

| Use | Not | Notes |
|---|---|---|
| NVIDIA Kubernetes Engine (NVKE) on first use, then NVKE | the pre-rename product name | Product name in prose. |
| NVIDIA Kubernetes Recipes (NVKR) on first use, then NVKR | the pre-rename build-system names, NKD | Build system. |
| operator | admin, SRE | The AI factory team that installs and runs NVKE. |
| customer | tenant, end user | The person or team that receives a workload cluster. |
| management cluster | site controller, site cluster, seed | The cluster that runs the management plane. |
| management plane | control plane (for NVKE itself), CWE | The NVKE API and controllers. |
| seed cluster | bootstrap cluster, kind cluster | The temporary cluster during bootstrap. |
| workload cluster | tenant cluster | A cluster NVKE creates for a customer. "NVKE cluster" is acceptable in overview prose. |
| node pool | node set, machine deployment | |
| infrastructure provider | infra provider, backend | NICo is the only one at preview. |
| NVIDIA Infra Controller (NICo) | Forge, Carbide | |

**Code literals keep their real names.** Commands, binaries, packages, flags,
API fields and repository names are written exactly as they exist in the code
(`nvkectl`, `nvke-agent`, `nvkr`, `cniPlugin`, `dsx-ai-factory/nvke`), in
backticks. Never rename a literal to match the product name.

Do not describe implementation internals that operators do not act on: the
workflow engine, its database, the outbox, internal service names. Name an
internal component only when the reader has to run, configure or inspect it.

Do not mention internal NVIDIA programs, sites, teams, people, channels,
trackers or hostnames. Do not describe air-gapped or offline installation.

## 5. Markdown and Fern conventions

- One `#` title per page, sentence case, matching the `page:` name in
  `docs/index.yml`. Section headings are `##` and `###`, sentence case.
- GitHub-flavored Markdown only, so pages render on GitHub as well as the
  site: no Fern MDX components. Notes are blockquotes that start with a bold
  label: `> **Note:**`, `> **Warning:**`, `> **Preview:**`.
- Diagrams are Mermaid code blocks. Every diagram has one sentence above it
  saying what it shows.
- Link to other pages with relative paths to the `.md` file
  (from a page in `docs/workload-clusters/`, Prepare nodes is
  `../operate/prepare-nodes.md`). Use the target page's
  title as the link text. Do not link to a page that is not in the
  navigation; `fern check` treats broken links as errors.
- Code blocks have a language (`bash`, `yaml`, `json`, `text`). One command
  per block where the reader runs them separately.
- Placeholders are `<UPPER_SNAKE_CASE>` and are explained right after the
  block. Example values use `example-` names, RFC 1918 or documentation
  addresses, and `REDACTED` or `example-token` for anything resembling a
  credential. Never include a real hostname, IP address, token or key.
- Tables need a header row. Keep cells to one or two sentences.
- No `TODO`, no editor notes, no source lists, no draft banners in the page.

## 6. Verify every claim

Before you finish:

1. Every command, flag, field, default and output you show exists in the code
   at the current commit. Record the file and line for each in your summary,
   not in the page.
2. Every behavior you describe ("NVKE retries", "the cluster becomes Ready")
   is backed by code or a test. If you cannot find it, remove it or raise it
   as an open question.
3. Run the terminology check if your session has it, and search the page for
   the "Not" column above.
4. Every link resolves to a page in the navigation.
5. A new page has a row in `docs/README.md` and an entry in `docs/index.yml`.
6. `make docs-check` and `python3 -B hack/name-check.py --mode fail` pass.
   Both are merge-gate jobs; `make preflight` runs neither.

## 7. What to return

When an agent writes a page for someone else to review, return:

- **Summary**: two or three sentences on what the page covers.
- **Claims and evidence**: each command, field, default and behavior with
  `path:line`.
- **Changed from the source material**: what you dropped, merged or corrected,
  and why.
- **Open questions**: anything you could not verify or that needs a product
  decision. Do not resolve these silently in the page.
