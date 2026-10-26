# Rex problems that LazyRex works around

This file lists problems in Rex, the terminal that LazyRex runs on, that LazyRex observed and
works around. Each entry names the date, the Rex build, what was observed, the workaround, and
what LazyRex asks of Rex. Rex behavior that is not a problem is in docs/rex-protocol.md.

## The Linux package name "rex" is also an Ubuntu package

Observed 2026-10-26 on Ubuntu 24.04 (noble), Rex 0.0.1792593189-g3f4a0a, installed from the Rex
`.deb`.

- The Rex apt package is named `rex`. Ubuntu's universe repository has a different package with
  the same name: the Perl deployment tool Rex, version 1.14.3-1.
- apt compares the versions, and 1.14.3-1 is higher than 0.0.x. So `apt upgrade` and
  unattended-upgrade replace the terminal Rex with the Perl tool. `/var/log/apt/history.log`
  recorded `Upgrade: rex:amd64 (0.0.1792593189-g3f4a0a, 1.14.3-1)`.
- After the upgrade, `/usr/bin/rex` was the Perl tool, and the `xterm-rex` terminfo entry was
  gone. A Rex server that already ran kept running from its old binary. Each new `rex` command
  started the Perl tool, so labs that start a Rex server failed.

Workaround: install the Rex `.deb` again (`apt-get install --allow-downgrades`), then
`apt-mark hold rex`. The hold also blocks upgrades to newer Rex builds until it is removed.

Ask: a package name that no distribution uses, or an epoch in the package version.

