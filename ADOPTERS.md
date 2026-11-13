# Adopters

This file lists organizations that use NVKE today, in production or in
development and test. Listing is voluntary. An entry records that an
organization uses NVKE. It does not imply endorsement by NVIDIA or by the
listed organization.

## Adopters list

| Organization | Type | Adoption level | Description of use | Since |
|---|---|---|---|---|

The list is empty. See [Add your organization](#add-your-organization).

## Adopter types

An organization can be more than one type. List every type that applies in one
row.

| Type | Description |
|---|---|
| End-User Organization | Uses NVKE internally, or builds on it, and does not sell it as a service. |
| Service Provider | Runs NVKE as a core component of a service it sells, such as a managed GPU cloud. |
| Project | An open-source project that integrates with or depends on NVKE. |
| Consultancy | Helps other organizations build solutions with NVKE. |

## Adoption levels

| Level | Description |
|---|---|
| Production | Runs NVKE for production workloads. |
| Dev/Test | Runs NVKE for development or test environments. |

An organization that is evaluating or trialing NVKE, and has not deployed it,
is not listed yet.

## Add your organization

Open a pull request that adds one row to the table. Write `Since` as the year
and month the organization started using NVKE, in `YYYY-MM` form.

- To list the organization by name, the organization must approve being named,
  and the pull request must say how that approval was given. A maintainer
  confirms it before merging. Without approval, list the organization
  anonymously.
- To stay anonymous, give an industry in place of a name, such as
  "Financial services company", and keep the description general. The pull
  request shows its author's account and commit email, so check that your
  GitHub profile and commits do not name the organization, or ask a
  maintainer to open the pull request for you.
- Do not include confidential or identifying details: customer names, site
  locations, cluster sizes or internal project names. Git history keeps every
  published version, so removing an entry later does not remove it from the
  history.
- A service provider lists itself only. Do not list your customers; they adopt
  NVKE through your service.
- Do not put contact emails or personal handles in the table.
- Do not add logos.

A row looks like this. It is an example, not an adopter:

```markdown
| Financial services company | End-User Organization | Dev/Test | GPU clusters for internal model training. | 2026-01 |
```

## Update or remove an entry

Open a pull request or an issue to change or remove your organization's entry.
Maintainers remove an entry on request, with no questions asked.
