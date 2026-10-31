# wolp

[![CI](https://github.com/Gurey/wolp/actions/workflows/ci.yml/badge.svg)](https://github.com/Gurey/wolp/actions/workflows/ci.yml)

**wolp** is a TCP proxy for machines that sleep. Clients connect to the proxy.
If the machine behind it doesn't answer, wolp sends a Wake-on-LAN packet and
holds the connection until the machine is up, then forwards it as usual.

**wolpa** is the optional agent that runs on each machine. When nothing has
used the machine for a while, wolp asks wolpa to put it to sleep. wolpa refuses
if the machine is busy in ways the proxy can't see: an SSH session, a GPU job,
or a direct connection.

wolp also serves a live status page showing each machine's state, a sleep
countdown, wake times and recent events. The same data is available as JSON at
`/api/status`.

## Download

Get an archive for your platform from the
[releases page](https://github.com/Gurey/wolp/releases). Each archive contains
the binaries, example configs and systemd units. Or build from source with Go:

```sh
go install github.com/Gurey/wolp/cmd/wolp@latest
go install github.com/Gurey/wolp/cmd/wolpa@latest
```

## Install the proxy (wolp)

Run it on a machine that stays on, such as a Raspberry Pi, on the same LAN as
the machines it wakes.

```sh
tar xzf wolp-*-linux-armv7.tar.gz && cd wolp-*/
sudo install -m 755 wolp /usr/local/bin/
sudo useradd --system --no-create-home wolp
sudo mkdir -p /etc/wolp
sudo cp wolp.toml /etc/wolp/          # then edit it: MAC, address, ports
sudo cp wolp.service /etc/systemd/system/
sudo systemctl enable --now wolp
```

Use IP addresses in the config, not `.local` names. A sleeping machine doesn't
answer mDNS.

## Install the agent (wolpa)

Run it on each machine that should sleep when idle. It runs as root because it
suspends the machine.

```sh
sudo install -m 755 wolpa /usr/local/bin/
sudo mkdir -p /etc/wolpa
sudo cp wolpa.toml /etc/wolpa/        # then edit it; starts in dry_run mode
openssl rand -hex 32 | sudo tee /etc/wolpa/wolpa.token >/dev/null
sudo chmod 600 /etc/wolpa/wolpa.token
sudo cp wolpa.service /etc/systemd/system/
sudo systemctl enable --now wolpa
```

Copy the token to the proxy machine, for example to `/etc/wolp/server.token`,
readable by the `wolp` user, and set the host's `agent_token_file` to it.
Relative paths in either config are resolved from the config file's directory.

Keep `dry_run = true` until the agent's log shows it would only suspend when
you expect it to.

## Which archive

| Archive | For |
|---|---|
| `linux-amd64` | most PCs and servers |
| `linux-arm64` | Raspberry Pi 3/4/5 with a 64-bit OS |
| `linux-armv7` | Raspberry Pi 2/3/4 with a 32-bit OS |
| `linux-armv6` | Raspberry Pi Zero/1 |
| `darwin-*` | macOS, proxy only |

Check a download against `SHA256SUMS`. `wolp -version` prints the version, the
build number and the commit, for example `wolp v0.1.0 build 3 (1a2b3c4)`.

## Build

```sh
make test   # vet and race-enabled tests
make build  # wolp and wolpa for this machine
make dist   # archives for every platform in dist/
```

## Releases

Pushing a version tag makes GitHub Actions test, build and publish a release.
The build number is the workflow's run number.

```sh
git tag v0.2.0
git push origin v0.2.0
```

Tags with a suffix, like `v0.2.0-rc1`, are published as pre-releases.

## License

MIT
