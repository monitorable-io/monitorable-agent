#!/bin/sh
# Served at the old R2 install.sh URL once this environment's installer host serves the
# real installer (docs/superpowers/specs/2026-09-26-installer-signing-design.md §4.4).
# It installs nothing.
printf '%s\n' "The Monitorable installer has moved. Run the command from your dashboard, or:" >&2
printf '%s\n' "  curl --proto '=https' --tlsv1.2 -fsSL @@INSTALLER_URL@@/install.sh | sudo sh -s -- <the same options>" >&2
exit 1
