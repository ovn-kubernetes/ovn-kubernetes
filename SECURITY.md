# Security Policy

## Scope

This policy covers vulnerabilities in OVN-Kubernetes itself, that is the code in this repository.

Vulnerabilities in the Go dependencies listed in `go.mod` are owned by the projects that publish
them and are not handled through this process. They are fixed by bumping the affected dependency
through a regular pull request, the same as any other dependency update. The repo uses
[dependabot](https://docs.github.com/en/code-security/dependabot/dependabot-security-updates/configuring-dependabot-security-updates),
which scans `go.mod` for known vulnerabilities and opens those pull requests automatically.

## Reporting a Vulnerability

Vulnerabilities are reported through GitHub
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability).
The report is private: only you, the [Maintainers](MAINTAINERS.md) and anyone later added to the
advisory as a collaborator can see it.
Where the button is and what it is called depends on your role in the project:

- **Maintainers**: open [Security → Advisories](https://github.com/ovn-kubernetes/ovn-kubernetes/security/advisories)
  and click **New draft security advisory**. GitHub does not show the report button to people who can
  manage advisories.
- **Members**: open [Security → Advisories](https://github.com/ovn-kubernetes/ovn-kubernetes/security/advisories)
  and click **Report a vulnerability**.
- **Everyone else**: open the repository [Security tab](https://github.com/ovn-kubernetes/ovn-kubernetes/security)
  and click **Report a vulnerability**.

Fill in the form. If you do not see the button, check that you are on `ovn-kubernetes/ovn-kubernetes`
and not on a fork, and that you are signed in.

### What happens next

We will endeavour to respond within 48 hours of reporting.
If a vulnerability is reported but considered low priority, it may be converted into an issue and handled on the public issue tracker.
The reporter is expected to take part in that decision and in the handling of the report in general: we
rely on reporters to answer questions, review the proposed fix and reply within a reasonable time. If a
reporter does not respond, we may proceed with triage, the fix and disclosure without them.
When the maintainers confirm a vulnerability is severe, we will endeavour to patch it within 48 hours of that confirmation and will update the reporter if that target cannot be met. We may ask you to collaborate with us on a temporary private fork of the repository.
Fixes are prepared for `master` and for every supported release branch, and are disclosed together
with the advisory. Before publishing, we may allow extra time for [adopters](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/ADOPTERS.md) that ship
OVN-Kubernetes to prepare their own releases, so that users are not left exposed once the details become public.

## Handling a Vulnerability

Vulnerabilities are handled following the GitHub
[coordinated disclosure](https://docs.github.com/en/code-security/concepts/vulnerability-reporting-and-management/coordinated-disclosure)
process: the report, the fixes and the disclosure are worked on privately between the reporter and the
Maintainers, and published together once fixes for all supported release branches are available. A fix
that is published for one branch discloses the vulnerability for all of them, so no branch is fixed in
public ahead of the others.

Security advisories are managed by the [Maintainers](MAINTAINERS.md) on
[Security → Advisories](https://github.com/ovn-kubernetes/ovn-kubernetes/security/advisories).

1. Reports filed by users appear in the advisories list in the **Triage** state. Open the report and
   either **Accept and open as draft** to keep working on it privately, or close it with a comment explaining
   why it is not a security risk.
2. Fill in the affected versions, patched versions and severity. Use **Add collaborators** to include people
   outside the project without giving them repository access; the reporter is added automatically.
3. Click **Start a temporary private fork** to develop and review the fixes outside of public view. Open one
   pull request in the fork per target branch: `master` and each supported `release-*` branch, with the fix
   backported as needed. CI does not run on the private fork, so test each fix locally. We also expect
   adopters that receive the fixes early to run them through CI on their private infrastructure,
   so that CI failures caused by the fix are found before it becomes public.
4. Click **Request CVE** to have GitHub assign a CVE identifier. The identifier stays reserved and
   unpublished until the advisory is published.
5. Before releasing the fix, consider privately notifying [adopters](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/ADOPTERS.md) that ship OVN-Kubernetes.
   Add their maintainers as collaborators on the advisory so they can pull the fixes from the private fork,
   and agree on a disclosure date that gives them time to prepare their own releases.
6. Once the fixes for all target branches are ready, click **Merge pull requests** in the advisory. All open
   pull requests in the fork are merged at once and the commits become public at this point, so do this only
   when every branch has a reviewed fix and the patch releases carrying them can be cut immediately
   afterwards.
7. Cut a patch release from every supported release branch that received a fix, so that a patched version
   exists for each supported release before the advisory is published. Record these versions as the patched versions in the advisory.
8. Click **Publish advisory**. This makes the advisory public, deletes the temporary private fork and submits
   the advisory to the [GitHub Advisory Database](https://github.com/advisories), which after review may
   notify dependent projects through Dependabot.
