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
- The runner's single-use registration is readable from the machine's instance metadata by any process on it until the
  runner has used it.
