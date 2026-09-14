# GUI

The web dashboard for the CB0401 Tune + Control. Written in Go and compiled to
a single native binary per OS — no runtime dependency at all for whoever
runs it, only the Go toolchain to build it, once.

It still shells out to the system `ssh` (and `sshpass` for the password
fallback) rather than reimplementing the SSH protocol - those are already
required elsewhere in this toolkit (opening SSH access in the first place
needs a real SSH client), so this doesn't add a new dependency, it just
means nothing here needs Python either.

## Building

Requires the [Go toolchain](https://go.dev/dl/) (1.21+) - only to build,
not to run the result:

```bash
./build.sh                 # cross-compiles for macOS (arm64 + Intel) and Windows into dist/
# or, for just your own machine:
go build -o cb0401-tune-control .
```

## Running

```bash
./start_gui.sh       # macOS/Linux - builds on first run if needed, then launches
./start_gui.ps1       # Windows
```

`setup.sh`/`setup.ps1` normally do this for you, generating `router_key`/`router_key.pub` and `.env` (`ROUTER_IP`, `ROUTER_ROOT_PASSWORD`, `NOTIFY_BACKEND`, `NTFY_TOPIC`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`) next to the binary — see the main [README](../README.md) for the full setup flow.
