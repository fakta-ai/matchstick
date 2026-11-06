# Security

matchstick creates cloud machines and holds a GitHub credential that registers runners, so a flaw in it can give
someone compute in your cloud or a place to run code in your CI. Please report vulnerabilities privately.

## Reporting

Use GitHub's private vulnerability reporting: the **Security** tab of this repository, then **Report a
vulnerability**. Please include what is affected, how to reproduce it, and what an attacker gains. Do not open a
public issue.

You will get an answer within a week. Fixes are released as a new version, with the report credited unless you ask
otherwise.

## Supported versions

The latest release. matchstick is young; fixes are not backported.

## Scope notes

- Self-hosted runners should run only workflows you trust. A runner's machine is deleted after its job, but anything
  the machine's network and identity reach during the job is reachable by that job's code.
- No secret is put in a machine's cloud settings (on Azure: user data, custom data, tags, extensions, OS profile),
  which anyone allowed to read the machine can read. The machine claims its runner's single-use registration from
  matchstick's claim endpoint with a document its cloud signs, and matchstick releases it once, only to the machine it
  was made for, before that machine's registration timeout. Any process on the machine can make that claim before the
  runner does; run nothing there before the claim but the image's own boot code.
- The claim endpoint's TLS key exists only in matchstick's memory; machines trust only its certificate.
- A report that matchstick released a registration to anything but its own machine, or released one twice, is in
  scope.
