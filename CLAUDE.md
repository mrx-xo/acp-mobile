# acp-mobile

Mobile web frontend for ACP (Agent Communication Protocol) sessions.

## Debugging

Start acp-mobile locally in test mode on a separate port:

```bash
go run . --test-mode 18091
```

`--test-mode` skips Origin header checks on WebSocket, so you can connect with curl/python/websocat.

### Fake replay server

To test replay scenarios, use a fake socket server that sends crafted messages:

1. Write a Go program that listens on a Unix socket in `$XDG_RUNTIME_DIR/acp-multiplex/<pid>.sock`
2. Send JSON-RPC messages matching the ACP protocol
3. acp-mobile discovers sockets by PID, so the socket filename must match the server's PID
4. Connect via WebSocket: `ws://127.0.0.1:18091/ws?sock=<pid>`

For the deterministic thought-rendering fixture, run these from the repository in two
terminals:

```bash
go run ./testdata/thought-fixture-server.go
go run . --test-mode 18091
```

Open the authenticated URL printed by `acp-mobile`, select `TEST: Thought Rendering`,
watch the split progress records stream in, and reload the page to exercise completed
replay. The fixture uses fixed IDs and payloads from `testdata/thought-replay.jsonl`; it
does not contact a live agent or external API.

### Connecting to replay via WebSocket

```python
import websocket, json
ws = websocket.create_connection("ws://127.0.0.1:18091/ws?sock=<pid>", timeout=3)
while True:
    msg = json.loads(ws.recv())
    print(json.dumps(msg, indent=2)[:200])
```

### Deploying changes on MrX

Production runs locally from `~/.local/bin/acp-mobile` under the launchd job
`com.marcosandrade.acp-mobile` on port 8090. Do not copy files to a separate host.

1. Commit the reviewed changes on `syzygy` and push them to `fork`.
2. Update `ACP_MOBILE_COMMIT` and its adjacent history comment in
   `~/.dotfiles/macos/syzygy/build-acp-tools.sh` to the exact pushed commit.
3. Run `~/.dotfiles/macos/syzygy/build-acp-tools.sh`. It builds the pinned
   commits into `~/.local/bin` and kickstarts the launchd job when it is loaded.
4. The build script leaves this checkout detached at the pin; switch it back to
   `syzygy` after verifying the live service.
