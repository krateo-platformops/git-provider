# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a suspected vulnerability.**

Use GitHub's private vulnerability reporting, which is enabled on this repository:
**[Report a vulnerability](https://github.com/krateo-platformops/git-provider/security/advisories/new)**
(Security → Advisories → Report a vulnerability).

A report is most useful when it says what an actor who holds a given permission can end up doing —
the permission they start from, the resource they reach, and what leaves the cluster. A runnable
manifest is welcome but never required; please hold it back from anywhere public.

## Supported versions

Fixes land on `main` and ship in the next tagged release. Only the latest minor is supported.

## What this provider can reach, by design

git-provider reconciles `Repo` and `LocalResource` custom resources. Two properties are worth
knowing when assessing a report, because they shape most of the interesting reports:

- **It acts with its own identity.** Reads and git pushes use the provider's ServiceAccount, not the
  identity of whoever authored the custom resource. The provider holds a cluster-wide `get`, because
  a `LocalResource` may reference arbitrary resource kinds.
- **It is an egress path.** A `LocalResource` names the git remote it pushes to, so anything the
  provider can read and serialise can leave the cluster.

Those two together mean that "who may create a `LocalResource` in namespace X" is a meaningful
privilege boundary. `fromRef` is confined to the referencing resource's own namespace for exactly
this reason (#10). A same-namespace read is still performed with the provider's identity rather than
the author's; authorizing it against the author (`SubjectAccessReview`) remains open work, and a
report that sharpens that case is welcome.
