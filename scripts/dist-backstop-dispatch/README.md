# dist-backstop trigger

A systemd timer that starts `.github/workflows/dist-backstop.yml` (env `prod`) every 15
minutes through GitHub's workflow-dispatch API. It runs on a host **outside our
infrastructure** (currently the Oracle Cloud VM `instance-arm-1`) and replaces GitHub's own
cron, which dropped almost every slot. The workflow does the checking and pings
healthchecks.io (check `installer-dist-prod`). If this trigger stops for any reason (host
down, token expired or revoked, GitHub refusing), no run starts, and healthchecks.io pages
on the missed ping.

| File | Installed at |
|---|---|
| `../dispatch-dist-backstop.sh` | `/usr/local/lib/dist-backstop/dispatch-dist-backstop.sh` (root, 0755) |
| `dist-backstop-dispatch.service` | `/etc/systemd/system/` |
| `dist-backstop-dispatch.timer` | `/etc/systemd/system/` |
| the token (never in git) | `/etc/dist-backstop/github-token` (root, 0600) |

## Token

A fine-grained personal access token:

- resource owner `monitorable-io`, repository access **only** `monitorable-agent`;
- repository permission **Actions: Read and write** (Metadata: read is added
  automatically). Nothing else.

Someone holding it can start, cancel, re-run or disable this repo's workflows, and delete
their logs. Every one of those stops or skips the backstop's runs, which healthchecks.io
reports as a missed ping. It can't read secrets, push code, or touch other repos. Write
the token on one line to `/etc/dist-backstop/github-token`. The service hands it to a
throwaway user through `LoadCredential`, and the script sends it to curl on stdin, never in
argv.

When it expires, dispatches answer HTTP 401, the backstop's check goes DOWN, and the
journal says `dispatch refused: HTTP 401`. To rotate, overwrite the file; nothing needs
restarting.

## Install

```bash
install -d -m 0755 /usr/local/lib/dist-backstop
install -m 0755 scripts/dispatch-dist-backstop.sh /usr/local/lib/dist-backstop/
install -m 0644 scripts/dist-backstop-dispatch/dist-backstop-dispatch.{service,timer} /etc/systemd/system/
install -d -m 0700 /etc/dist-backstop     # then write the token to github-token, mode 0600
systemctl daemon-reload
systemctl start dist-backstop-dispatch.service   # one dispatch now; check the journal
systemctl enable --now dist-backstop-dispatch.timer
```

## Operate

```bash
systemctl list-timers dist-backstop-dispatch.timer
journalctl -u dist-backstop-dispatch.service -n 20   # "dispatched …" or "dispatch refused: HTTP …"
gh run list -R monitorable-io/monitorable-agent -w dist-backstop -e workflow_dispatch -L 10
```

Test: `scripts/test-dispatch-dist-backstop.sh`, which runs in `validate.yml`.
