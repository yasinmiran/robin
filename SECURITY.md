# Security policy

Robin hands a browser tab the keyboard, pointer and screen of a Linux
desktop, so a flaw in it reaches further than its size suggests. Reports are
welcome, in private: please do not open an issue, discussion or pull request
for a vulnerability.

## Supported versions

There is no tagged release yet. Until the first one, fixes land on `main`
and `main` is the only supported version. Once releases exist, the latest
release and `main` get security fixes; an older release does not, and the
fix for it is to upgrade. Every release binary is reproducible from its tag
(see [Verifying a release](README.md#verifying-a-release)), so a fix can be
checked against the commit that claims it.

| Version | Supported |
| --- | --- |
| `main` | yes |
| latest release | yes, from the first release on |
| older releases | no |

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: open the repository's
**Security** tab and choose **Report a vulnerability**, or go straight to
<https://github.com/yasinmiran/robin/security/advisories/new>. The report
stays between you and the maintainer until an advisory is published.

What helps most is the path a flaw takes: which side starts it (another
user on the host, another user on the client machine, the network, a web
page, the host itself), what it reaches, and the commit or release you
read. A failing test, or the steps that show the behaviour against your own
machines, beats a description. Please do not test against hosts or
accounts that are not yours.

## What to expect

Robin is maintained by one person, so these are targets, not a contract:

- an acknowledgement within 7 days;
- a first assessment, in scope or not and how bad, within 14 days;
- a fix, or a plan with a date, within 90 days of the report, sooner for
  anything that lets another account in.

The fix ships with a regression test and a GitHub security advisory, and a
CVE where one applies. You are credited in the advisory unless you would
rather not be. Please keep the details private until the advisory is out,
or until 90 days have passed, whichever comes first; if a fix needs longer,
we agree on a date together.

## Scope

The threat model is the [Security](README.md#security) section of the
README, which says what each surface promises. A way to break one of those
promises is in scope. In particular:

- another user of the host reaching your session: the agent's ICE-TCP
  listener, its run directory and unix sockets, the X display and its
  cookie, the sound socket, the guests' door, a kept session's daemon;
- another user of the client machine reaching your session: the page and
  its websocket, the launch link, the token, the ICE-TCP tunnel's loopback
  port, the macros file;
- the network path standing in for the agent or the browser (the DTLS
  fingerprint pinned over SSH), or making either connect somewhere it
  should not;
- a web page in the same browser driving the page or its websocket;
- a hostile or compromised host attacking the client: escape sequences in
  what the CLI prints, memory the CLI or page can be made to hold, files
  or macros written outside where the README says they go;
- the page or a guest doing more on the host than the README allows:
  files outside the download folder, files read without a click,
  input or clipboard from a guest without control, a private session that
  leaves the desktop changed after the agent dies;
- the build: a release binary that does not match its tag, a toolchain
  download that is not checked against its pinned hash.

Out of scope:

- anything that needs your account already, on the host or on the client:
  a process running as you can do what Robin does without it;
- a compromised SSH setup, `ssh` binary, browser or operating system;
- what the README states as a limit: the STUN server learning your
  addresses, the curtain not locking the keyboard, the paste a Wayland
  host's clipboard briefly holds, a v4l2loopback camera readable by its
  device's group, and the rest of [Known limits](README.md#known-limits);
- the test harness (`e2e/`, the `testdata` containers) and the docs site,
  unless a flaw there reaches a user of Robin;
- a vulnerable dependency whose flaw Robin's code never reaches.
  Dependabot and govulncheck in CI cover those; a report that shows it
  reachable is in scope.
